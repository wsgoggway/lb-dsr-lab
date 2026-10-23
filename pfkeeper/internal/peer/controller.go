package peer

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	pfkeeperv1 "github.com/wsgoggway/lb-dsr-lab/pfkeeper/api/pfkeeperv1"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/health"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/metrics"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/reconcile"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/render"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

// Loop intervals of the controller.
const (
	DefaultTickInterval = 10 * time.Second
	DefaultPushInterval = 30 * time.Second
	// warnInterval throttles the "peer unreachable" log line.
	warnInterval = time.Minute
)

// ConfigApplier is the slice of reconcile.ConfigReconciler the controller
// needs (interface so tests can inject a fake).
type ConfigApplier interface {
	Reconcile(ctx context.Context, st *state.State, revision string, healthy reconcile.HealthyView) (reconcile.ReconcileResult, error)
}

// HealthApplier is the health-layer slice the controller needs to prune
// freshly applied snapshots (satisfied by *reconcile.HealthController).
type HealthApplier interface {
	ApplyEffective(ctx context.Context, table string, desired []string, localView, peerView map[string]string, peerReachable bool) (reconcile.EffectiveResult, error)
}

// PeerState is the latest observation of the peer.
type PeerState struct {
	Reachable    bool
	PeerRevision string
	Diverged     bool
	Degraded     bool // peer unreachable: BACKUP keeps its last snapshot
	CheckedAt    time.Time
}

// Controller runs the peering loop for one node.
//
//   - every TickInterval it SyncChecks the peer (own hash/revision);
//   - MASTER: pushes a config snapshot on revision/hash change, on divergence
//     (heal) or every PushInterval; always publishes the local health view;
//   - BACKUP: never touches git; it applies snapshots delivered by the MASTER
//     (ApplySnapshot, called from the M6 PeeringService server) and publishes
//     its own health view so the MASTER can evaluate and-consensus;
//   - when the peer is unreachable the loop degrades: BACKUP keeps operating
//     on the last snapshot and the warning is logged at most once a minute.
type Controller struct {
	role    RoleSource
	self    string
	cli     *PeerClient
	applier ConfigApplier
	// hc prunes health-excluded members right after a snapshot lands: the
	// snapshot is applied as full desired, and without an immediate
	// effective-set pass the BACKUP would flap until the next local state
	// change.
	hc    HealthApplier
	store *state.Store
	log   *slog.Logger

	TickInterval time.Duration
	PushInterval time.Duration
	// Metrics, when set, receives the peer observation on every loop tick
	// (peer_reachable / peer_diverged / peer_lag_seconds — SetPeer was dead
	// code before v1.1).
	Metrics *metrics.Metrics

	mu              sync.Mutex
	localView       map[string]string // "table/addr" -> UP|DOWN|INACTIVE
	peerView        map[string]string // peer-published view, same key format
	lastSnapshot    *pfkeeperv1.EffectiveConfigSnapshot
	appliedHash     string // hash of the snapshot applied last (BACKUP identity)
	appliedRev      string // revision of the snapshot applied last
	healRequested   bool   // set by RequestHeal; next tick force-pushes
	lastPushHash    string
	lastPushRev     string
	lastPushAt      time.Time
	lastWarn        time.Time
	lastReachableAt time.Time
	peer            PeerState
}

// NewController wires the peering controller. peerAddr is kept for
// diagnostics; the connection itself lives in cli.
func NewController(role RoleSource, self, peerAddr string, cli *PeerClient, applier ConfigApplier, hc *reconcile.HealthController, store *state.Store) *Controller {
	// Normalize: a nil *HealthController must NOT become a typed-nil
	// interface in the hc field (that would defeat the nil check in
	// applyHealth and panic on the nil receiver).
	var health HealthApplier
	if hc != nil {
		health = hc
	}
	return &Controller{
		role:         role,
		self:         self,
		cli:          cli,
		applier:      applier,
		hc:           health,
		store:        store,
		log:          slog.Default(),
		TickInterval: DefaultTickInterval,
		PushInterval: DefaultPushInterval,
		localView:    map[string]string{},
		peerView:     map[string]string{},
	}
}

// SetLogger overrides the default slog logger (the daemon injects its own).
func (c *Controller) SetLogger(l *slog.Logger) {
	if l != nil {
		c.log = l
	}
}

// SetLocalView updates the local health view; keys follow the
// "table/address" convention (LocalViewKey).
func (c *Controller) SetLocalView(view map[string]string) {
	cp := make(map[string]string, len(view))
	for k, v := range view {
		cp[k] = v
	}
	c.mu.Lock()
	c.localView = cp
	c.mu.Unlock()
}

// SetPeerView stores the peer-published health view (PeeringService/
// PublishHealth handler on the receiving side).
func (c *Controller) SetPeerView(view map[string]string) {
	cp := make(map[string]string, len(view))
	for k, v := range view {
		cp[k] = v
	}
	c.mu.Lock()
	c.peerView = cp
	c.mu.Unlock()
}

// PeerView returns the last peer-published health view ("" map when none).
func (c *Controller) PeerView() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.peerView))
	for k, v := range c.peerView {
		out[k] = v
	}
	return out
}

// Status is the peer observation surfaced through StatusService/PeerState.
type Status struct {
	Reachable    bool
	PeerRevision string
	Diverged     bool
	Degraded     bool
	LagSeconds   int64 // seconds since the peer was last seen reachable
	CheckedAt    time.Time
}

// Snapshot returns the latest peer observation for StatusService.
func (c *Controller) Snapshot() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	lag := int64(0)
	if c.lastReachableAt.IsZero() {
		lag = -1 // never seen reachable
	} else if !c.peer.Reachable {
		lag = int64(time.Since(c.lastReachableAt).Seconds())
	}
	return Status{
		Reachable:    c.peer.Reachable,
		PeerRevision: c.peer.PeerRevision,
		Diverged:     c.peer.Diverged,
		Degraded:     c.peer.Degraded,
		LagSeconds:   lag,
		CheckedAt:    c.peer.CheckedAt,
	}
}

// PeerStatus returns the latest peer observation (safe for concurrent reads).
func (c *Controller) PeerStatus() PeerState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peer
}

// LastSnapshot returns the snapshot applied last (nil when none yet).
func (c *Controller) LastSnapshot() *pfkeeperv1.EffectiveConfigSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSnapshot
}

// Run drives the loop until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) {
	tick := c.TickInterval
	if tick <= 0 {
		tick = DefaultTickInterval
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.tickOnce(ctx)
		}
	}
}

// SyncOnce performs exactly one peering iteration synchronously (used by
// tests and by the daemon for an immediate first check).
func (c *Controller) SyncOnce(ctx context.Context) { c.tickOnce(ctx) }

// tickOnce performs one peering iteration. Tests call it directly instead of
// waiting on tickers, so the library itself never sleeps.
func (c *Controller) tickOnce(ctx context.Context) {
	hash, rev := c.selfIdentity(ctx)

	resp, err := c.cli.SyncCheck(ctx, hash, rev)
	now := time.Now()
	if err != nil {
		c.mu.Lock()
		peerRev := c.peer.PeerRevision
		c.peer = PeerState{Reachable: false, Degraded: true, PeerRevision: peerRev, CheckedAt: now}
		shouldWarn := now.Sub(c.lastWarn) >= warnInterval
		if shouldWarn {
			c.lastWarn = now
		}
		c.mu.Unlock()
		if shouldWarn {
			c.log.Warn("peer unreachable, running degraded", "self", c.self, "peer", c.cli.PeerID, "error", err)
		}
		c.publishPeerMetrics()
		return
	}

	c.mu.Lock()
	c.peer = PeerState{
		Reachable:    true,
		PeerRevision: resp.GetPeerRevision(),
		Diverged:     resp.GetDiverged(),
		CheckedAt:    now,
	}
	c.lastReachableAt = now
	healRequested := c.healRequested
	c.healRequested = false
	c.mu.Unlock()
	c.publishPeerMetrics()

	role := Role(c.role.Role())
	switch {
	case role == RoleMaster && (resp.GetDiverged() || healRequested):
		c.log.Warn("peer diverged from us, re-pushing snapshot", "self", c.self, "peer_rev", resp.GetPeerRevision())
	case role == RoleBackup && resp.GetDiverged():
		c.log.Warn("we diverged from the master, awaiting PushConfig", "self", c.self, "our_rev", rev, "master_rev", resp.GetPeerRevision())
	}

	if role == RoleMaster {
		c.maybePush(ctx, hash, rev, resp.GetDiverged() || healRequested, now)
	}
	// Both roles publish their health view: the MASTER needs the BACKUP's
	// view for and-consensus, and the BACKUP serves the MASTER's Diff/Status.
	if err := c.cli.PublishHealth(ctx, c.healthView()); err != nil {
		c.log.Warn("publish health failed", "self", c.self, "error", err)
	}
}

// publishPeerMetrics pushes the latest peer observation into the metrics
// registry (reachable from the SyncCheck outcome, diverged/lag from the
// response and the last-reachable bookkeeping). Snapshot takes mu briefly —
// never held across I/O.
func (c *Controller) publishPeerMetrics() {
	if c.Metrics == nil {
		return
	}
	s := c.Snapshot()
	c.Metrics.SetPeer(s.Reachable, s.Diverged, s.LagSeconds)
}

// SetAppliedSnapshot records the snapshot applied last: its hash/revision
// become this node's SyncCheck identity on the BACKUP (the local git dir is
// stale there by design — the BACKUP does not touch git).
func (c *Controller) SetAppliedSnapshot(hash, rev string) {
	c.mu.Lock()
	c.appliedHash, c.appliedRev = hash, rev
	c.mu.Unlock()
}

// RequestHeal makes the next loop tick force-push the snapshot to the peer
// regardless of change/age heuristics. The PeeringService SyncCheck handler
// on the MASTER calls it when the caller's hash does not match ours.
func (c *Controller) RequestHeal() {
	c.mu.Lock()
	c.healRequested = true
	c.mu.Unlock()
}

// SyncIdentity returns the hash/revision this node reports in SyncCheck:
// the applied snapshot on the BACKUP, the git state on the MASTER.
func (c *Controller) SyncIdentity(ctx context.Context) (hash, rev string) {
	return c.selfIdentity(ctx)
}

// selfIdentity returns the config hash and revision this node reports.
// The MASTER reads the store; the BACKUP reports the applied snapshot, and
// only falls back to the (stale, by design) local git state when no snapshot
// has been applied yet — the mismatch is what triggers the heal.
func (c *Controller) selfIdentity(ctx context.Context) (hash, rev string) {
	if Role(c.role.Role()) != RoleMaster {
		c.mu.Lock()
		hash, rev = c.appliedHash, c.appliedRev
		c.mu.Unlock()
		if hash != "" || rev != "" {
			return hash, rev
		}
		// No applied snapshot yet: report the stale local git state so the
		// MASTER sees the mismatch and heals.
	}
	if c.store != nil {
		if h, err := c.store.ConfigHash(ctx); err == nil {
			hash = h
		}
		if r, err := c.store.HeadRevision(ctx); err == nil {
			rev = r
		}
	}
	return hash, rev
}

// maybePush pushes a snapshot on change, divergence or age; callers ensure
// role == MASTER.
func (c *Controller) maybePush(ctx context.Context, hash, rev string, diverged bool, now time.Time) {
	pushEvery := c.PushInterval
	if pushEvery <= 0 {
		pushEvery = DefaultPushInterval
	}

	c.mu.Lock()
	changed := hash != c.lastPushHash || rev != c.lastPushRev
	due := now.Sub(c.lastPushAt) >= pushEvery
	c.mu.Unlock()

	if !diverged && !changed && !due {
		return
	}
	snap := c.BuildSnapshot(ctx, hash, rev)
	if err := c.cli.PushConfig(ctx, snap); err != nil {
		c.log.Warn("push config failed", "self", c.self, "error", err)
		return
	}
	c.mu.Lock()
	c.lastPushHash, c.lastPushRev, c.lastPushAt = hash, rev, now
	c.mu.Unlock()
}

// BuildSnapshot assembles the effective-config snapshot pushed to the BACKUP:
// desired tables and anchors from the store; hash/revision identify the
// applied git state. Tables without members are carried as empty strings so
// the BACKUP sees the full desired picture. The service anchors are rendered
// first (health-filtered by the local view) and merged into st.Anchors, so
// the BACKUP receives the effective weighted chains verbatim and its
// SyncCheck identity stays stable (anchors are part of the snapshot, not of
// the git hash).
func (c *Controller) BuildSnapshot(ctx context.Context, hash, rev string) *pfkeeperv1.EffectiveConfigSnapshot {
	snap := &pfkeeperv1.EffectiveConfigSnapshot{Revision: rev, ConfigHash: hash}
	if c.store == nil {
		return snap
	}
	st, err := state.LoadState(c.store.Dir)
	if err != nil {
		c.log.Warn("snapshot: load state failed", "dir", c.store.Dir, "error", err)
		return snap
	}
	if _, rerr := render.All(&st, c.healthyView()); rerr != nil {
		// Partial failures (a service with a dangling table reference) must
		// not block the push of the rest; the BACKUP's SyncCheck + heal loop
		// converges on the next pass.
		c.log.Warn("snapshot: render services failed", "error", rerr)
	}
	tables := make(map[string]string, len(st.Tables))
	for _, td := range st.Tables {
		tables[td.Name] = strings.Join(state.Addrs(td.Members), "\n")
	}
	snap.Tables = tables
	if len(st.Anchors) > 0 {
		snap.Anchors = st.Anchors
	}
	return snap
}

// healthyView converts the local health view into the renderer's filter:
// members not known DOWN are healthy (UP or not yet probed — mirrors
// Registry.HealthyStrict), so a total outage degrades the rendered chain
// instead of looking like "no health opinion".
func (c *Controller) healthyView() render.HealthyView {
	c.mu.Lock()
	local := c.localView
	c.mu.Unlock()

	out := map[string]map[string]bool{}
	for key, stt := range local {
		t, addr, ok := splitLocalViewKey(key)
		if !ok || stt == health.StateDOWN {
			continue
		}
		if out[t] == nil {
			out[t] = map[string]bool{}
		}
		out[t][addr] = true
	}
	return out
}

// ApplySnapshot converges the local pf to a snapshot received from the
// MASTER (the M6 PeeringService server calls this on the BACKUP only). The
// snapshot's anchors arrive already rendered by the MASTER and are applied
// verbatim (no local re-render — st carries no services); the BACKUP's
// health watcher re-renders them itself on local health transitions.
func (c *Controller) ApplySnapshot(ctx context.Context, snap *pfkeeperv1.EffectiveConfigSnapshot) (reconcile.ReconcileResult, error) {
	st := &state.State{Anchors: map[string]string{}}
	for name, members := range snap.GetTables() {
		td := state.TableDef{Name: name}
		for _, line := range strings.Split(members, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				td.Members = append(td.Members, state.PlainMember(line))
			}
		}
		st.Tables = append(st.Tables, td)
	}
	for name, rules := range snap.GetAnchors() {
		st.Anchors[name] = rules
	}
	// healthy = nil: the snapshot is applied verbatim as desired; the
	// BACKUP's health layer prunes unhealthy members on top of it, so the
	// layers never fight over membership.
	res, err := c.applier.Reconcile(ctx, st, snap.GetRevision(), nil)
	if err != nil {
		return res, err
	}
	c.mu.Lock()
	c.lastSnapshot = snap
	c.mu.Unlock()
	// Record the applied snapshot as our SyncCheck identity (the peering
	// server also calls SetAppliedSnapshot; this covers direct callers).
	c.SetAppliedSnapshot(snap.GetConfigHash(), snap.GetRevision())
	// The snapshot landed as full desired; converge the effective set to
	// desired ∩ localHealthy immediately instead of waiting for the next
	// local state change (otherwise the BACKUP flaps against every
	// periodic snapshot).
	c.applyHealth(ctx, st)
	return res, nil
}

// applyHealth re-applies the health-filtered membership for every table of
// the just-applied snapshot (point add/delete, never replace).
func (c *Controller) applyHealth(ctx context.Context, st *state.State) {
	if c.hc == nil {
		return
	}
	c.mu.Lock()
	local := c.localView
	c.mu.Unlock()
	peer := c.PeerView()
	peerReachable := c.Snapshot().Reachable
	for _, td := range st.Tables {
		if _, err := c.hc.ApplyEffective(ctx, td.Name, state.Addrs(td.Members),
			viewForTable(local, td.Name), viewForTable(peer, td.Name), peerReachable); err != nil {
			c.log.Warn("post-snapshot health apply failed", "table", td.Name, "error", err)
		}
	}
}

// viewForTable narrows a composite "table/addr" view down to the addr-keyed
// view of one table (HealthController.ApplyEffective's contract).
func viewForTable(view map[string]string, table string) map[string]string {
	out := make(map[string]string, len(view))
	for k, v := range view {
		if t, addr, ok := splitLocalViewKey(k); ok {
			if t == table {
				out[addr] = v
			}
			continue
		}
		out[k] = v
	}
	return out
}

// healthView converts the local view into the wire HealthView.
func (c *Controller) healthView() *pfkeeperv1.HealthView {
	c.mu.Lock()
	view := c.localView
	c.mu.Unlock()

	out := &pfkeeperv1.HealthView{Node: c.self}
	for key, st := range view {
		table, addr, ok := splitLocalViewKey(key)
		if !ok {
			table, addr = "", key
		}
		out.Members = append(out.Members, &pfkeeperv1.MemberHealth{
			Table:   table,
			Address: addr,
			State:   st,
		})
	}
	return out
}
