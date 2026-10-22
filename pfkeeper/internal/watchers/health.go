package watchers

import (
	"context"
	"log/slog"
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

	nextDue     map[string]time.Time
	lastSt      map[string]string // monitor key -> last published state
	published   bool
	lastPublish time.Time
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
// the startup/change/periodic rule, recompute effective membership on
// change. Exported via ProbeOnce for tests.
func (w *HealthWatcher) tickOnce(ctx context.Context, log *slog.Logger) {
	changed := w.tick(ctx, log)
	w.publish(changed)
	if changed {
		w.applyEffective(ctx, log)
	}
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

// ProbeOnce performs exactly one probing iteration synchronously (tests,
// and the daemon's startup path). Reports whether any member state changed.
func (w *HealthWatcher) ProbeOnce(ctx context.Context) bool {
	log := w.Log
	if log == nil {
		log = slog.Default()
	}
	changed := w.tick(ctx, log)
	w.publish(changed)
	if changed {
		w.applyEffective(ctx, log)
	}
	return changed
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
		for _, member := range td.Members {
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
// views (local registry + peer).
func (w *HealthWatcher) applyEffective(ctx context.Context, log *slog.Logger) {
	st := w.State()
	if st == nil {
		return
	}
	local := w.Reg.View()
	peerView, peerReachable := map[string]string{}, false
	if w.PeerView != nil {
		peerView, peerReachable = w.PeerView()
	}
	for _, td := range st.Tables {
		if len(td.Members) == 0 {
			continue
		}
		res, err := w.Hc.ApplyEffective(ctx, td.Name, td.Members, local, peerView, peerReachable)
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

// parseInterval falls back to def for empty/unparsable durations (validation
// rejects those upstream; the watcher stays defensive).
func parseInterval(v string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return def
}
