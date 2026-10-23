package watchers

import (
	"context"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/health"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/metrics"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/reconcile"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

// HealthWatcher owns the probe loop: it keeps the monitor registry in sync
// with the desired state, probes every member on its spec interval, folds
// results into the rise/fall monitors and, when any member state changed,
// recomputes the effective membership through the HealthController.
type HealthWatcher struct {
	Reg     *health.Registry
	Hc      *reconcile.HealthController
	Metrics *metrics.Metrics
	// Rec, when set, re-renders and re-applies the service anchors after a
	// health transition (weighted chains reference member addresses inline,
	// so a member flip must regenerate the chain, not only the table
	// membership).
	Rec *reconcile.ConfigReconciler
	// State returns the desired state (kept fresh by the git watcher).
	State func() *state.State
	// PeerView returns the peer's "table/addr" -> state view and whether the
	// peer is currently reachable.
	PeerView func() (map[string]string, bool)
	// Interval is the tick granularity (default 1s); per-member intervals
	// come from the health specs.
	Interval time.Duration
	// LocalSink, when set, receives the FULL local registry view on startup
	// (right after the first probe pass), on every member state change and
	// then at least every republishInterval — so the peer-published view can
	// never stay empty while members exist. Main wires it to
	// peer.Controller.SetLocalView.
	LocalSink func(map[string]string)
	Log       *slog.Logger

	nextDue           map[string]time.Time
	lastSt            map[string]string // monitor key -> last published state
	published         bool
	lastPublish       time.Time
	lastPeerView      map[string]string // peer view at the previous iteration
	lastPeerReachable bool
}

// republishInterval bounds how often the full view is re-published even
// without changes.
const republishInterval = 30 * time.Second

// Run drives the loop until ctx is cancelled. The first probe pass runs
// immediately (before the ticker) so the local view is populated — and
// published — right at startup.
func (w *HealthWatcher) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = time.Second
	}
	log := w.Log
	if log == nil {
		log = slog.Default()
	}
	w.tickOnce(ctx, log)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.tickOnce(ctx, log)
		}
	}
}

// tickOnce is one full iteration: probe due monitors, publish the view per
// the startup/change/periodic rule, recompute effective membership and
// re-render the service anchors on local state change OR peer-view change
// (the weighted chains depend on the health view). Reports whether any LOCAL
// member state changed.
func (w *HealthWatcher) tickOnce(ctx context.Context, log *slog.Logger) bool {
	changed := w.tick(ctx, log)
	peerChanged := w.trackPeerView()
	w.publish(changed)
	if changed || peerChanged {
		// Re-run the consensus when the MASTER changed its mind too, not
		// only on local state flips.
		w.applyEffective(ctx, log)
		w.applyServices(ctx, log)
	}
	return changed
}

// trackPeerView reports whether the peer-published view (or its
// reachability) changed since the previous iteration.
func (w *HealthWatcher) trackPeerView() bool {
	peer, reachable := map[string]string{}, false
	if w.PeerView != nil {
		peer, reachable = w.PeerView()
	}
	changed := !maps.Equal(peer, w.lastPeerView) || reachable != w.lastPeerReachable
	// Clone: callers may mutate their map in place; the stored snapshot must
	// not alias it.
	w.lastPeerView, w.lastPeerReachable = maps.Clone(peer), reachable
	return changed
}

// publish hands the full local view to LocalSink per the
// startup/change/periodic rule.
func (w *HealthWatcher) publish(changed bool) {
	if w.LocalSink == nil {
		return
	}
	now := time.Now()
	if !w.published || changed || now.Sub(w.lastPublish) >= republishInterval {
		w.LocalSink(w.Reg.View())
		w.published, w.lastPublish = true, now
	}
}

// ProbeOnce performs exactly one iteration synchronously — identical to a
// single Run-loop tick (tests, and the daemon's startup path). Reports
// whether any local member state changed.
func (w *HealthWatcher) ProbeOnce(ctx context.Context) bool {
	log := w.Log
	if log == nil {
		log = slog.Default()
	}
	return w.tickOnce(ctx, log)
}

// tick syncs the registry, probes due monitors and reports whether any
// member state changed.
func (w *HealthWatcher) tick(ctx context.Context, log *slog.Logger) bool {
	if w.nextDue == nil {
		w.nextDue = map[string]time.Time{}
	}
	if w.lastSt == nil {
		w.lastSt = map[string]string{}
	}
	st := w.State()
	if st == nil {
		return false
	}
	w.syncRegistry(st, log)

	now := time.Now()
	changed := false
	for _, m := range w.Reg.List() {
		key := m.Table + "/" + m.Address
		if due, ok := w.nextDue[key]; ok && now.Before(due) {
			continue
		}
		interval := parseInterval(m.Spec.Interval, 5*time.Second)
		w.nextDue[key] = now.Add(interval)

		prober := health.Pick(m.Spec)
		if prober == nil {
			continue
		}
		res := prober.Probe(ctx, m.Address, m.Spec)
		prev := m.State()
		m.Observe(res)
		w.Metrics.ObserveProbe(m.Table, m.Address, res.Passed, res.Reason)
		w.Metrics.SetMemberHealth(m.Table, m.Address, m.State())
		w.lastSt[key] = m.State()
		if m.State() != prev {
			log.Info("member state changed", "table", m.Table, "address", m.Address,
				"from", prev, "to", m.State(), "reason", res.Reason)
			changed = true
		}
	}
	return changed
}

// syncRegistry registers monitors for desired (table, member) pairs with a
// health spec and removes monitors that left the desired state.
func (w *HealthWatcher) syncRegistry(st *state.State, log *slog.Logger) {
	desired := map[string]bool{}
	for _, td := range st.Tables {
		if td.Health == nil {
			continue
		}
		for _, member := range state.Addrs(td.Members) {
			desired[td.Name+"/"+member] = true
			w.Reg.Register(td.Name, member, *td.Health)
		}
	}
	for _, m := range w.Reg.List() {
		if !desired[m.Table+"/"+m.Address] {
			if w.Reg.Remove(m.Table, m.Address) {
				log.Debug("monitor removed", "table", m.Table, "address", m.Address)
			}
		}
	}
}

// applyEffective recomputes membership per desired table using the consensus
// views (local registry + peer). Both views are keyed "table/addr"
// (Registry.View / SetPeerView), so they are narrowed to the addr-keyed
// per-table view ApplyEffective expects — passing the composite maps straight
// through made every member look DOWN and emptied the tables (t08).
func (w *HealthWatcher) applyEffective(ctx context.Context, log *slog.Logger) {
	st := w.State()
	if st == nil {
		return
	}
	local := w.Reg.View()
	peerFull, peerReachable := map[string]string{}, false
	if w.PeerView != nil {
		peerFull, peerReachable = w.PeerView()
	}
	for _, td := range st.Tables {
		if len(td.Members) == 0 {
			continue
		}
		res, err := w.Hc.ApplyEffective(ctx, td.Name, state.Addrs(td.Members),
			tableView(local, td.Name), tableView(peerFull, td.Name), peerReachable)
		if err != nil {
			log.Warn("effective apply failed", "table", td.Name, "error", err)
			continue
		}
		if len(res.Added) > 0 || len(res.Removed) > 0 {
			log.Info("effective membership updated", "table", td.Name,
				"added", res.Added, "removed", res.Removed)
		}
	}
}

// applyServices re-renders the service anchors against the current health
// view and re-applies the drifted ones (Rec wired only). The rendered plans
// are republished as the service_probability gauge; a Degraded plan (no
// active member) is logged as a warning — its anchor is intentionally empty.
func (w *HealthWatcher) applyServices(ctx context.Context, log *slog.Logger) {
	if w.Rec == nil {
		return
	}
	st := w.State()
	if st == nil {
		return
	}
	plans, applied, err := w.Rec.ApplyServices(ctx, st, registryHealthy(w.Reg))
	if err != nil {
		log.Warn("service anchor re-render failed", "error", err)
		return
	}
	// publishPlans also warns on Degraded plans (no active member).
	publishPlans(w.Metrics, plans, log)
	if applied > 0 {
		log.Info("service anchors re-applied after health change", "applied", applied)
	}
}

// tableView narrows a composite "table/addr"-keyed health view down to the
// addr-keyed view of one table (HealthController.ApplyEffective's contract).
// Plain address keys (no "/") are tolerated for robustness.
func tableView(view map[string]string, table string) map[string]string {
	out := make(map[string]string, len(view))
	for k, v := range view {
		t, addr, found := strings.Cut(k, "/")
		if !found {
			out[k] = v
			continue
		}
		if t == table {
			out[addr] = v
		}
	}
	return out
}

// parseInterval falls back to def for empty/unparsable durations (validation
// rejects those upstream; the watcher stays defensive).
func parseInterval(v string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return def
}
