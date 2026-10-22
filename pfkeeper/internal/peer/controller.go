package peer

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	pfkeeperv1 "github.com/wsgoggway/lb-dsr-lab/pfkeeper/api/pfkeeperv1"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/reconcile"
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
	Reconcile(ctx context.Context, st *state.State, revision string) (reconcile.ReconcileResult, error)
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
	// hc is the shared health layer, wired for M7 effective recomputation
	// after ApplySnapshot; intentionally unused in M5.
	hc    *reconcile.HealthController
	store *state.Store
	log   *slog.Logger

	TickInterval time.Duration
	PushInterval time.Duration

	mu           sync.Mutex
	localView    map[string]string // "table/addr" -> UP|DOWN|INACTIVE
	lastSnapshot *pfkeeperv1.EffectiveConfigSnapshot
	lastPushHash string
	lastPushRev  string
	lastPushAt   time.Time
	lastWarn     time.Time
	peer         PeerState
}

// NewController wires the peering controller. peerAddr is kept for
// diagnostics; the connection itself lives in cli.
func NewController(role RoleSource, self, peerAddr string, cli *PeerClient, applier ConfigApplier, hc *reconcile.HealthController, store *state.Store) *Controller {
	return &Controller{
		role:         role,
		self:         self,
		cli:          cli,
		applier:      applier,
		hc:           hc,
		store:        store,
		log:          slog.Default(),
		TickInterval: DefaultTickInterval,
		PushInterval: DefaultPushInterval,
		localView:    map[string]string{},
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
		return
	}

	c.mu.Lock()
	c.peer = PeerState{
		Reachable:    true,
		PeerRevision: resp.GetPeerRevision(),
		Diverged:     resp.GetDiverged(),
		CheckedAt:    now,
	}
	c.mu.Unlock()

	role := Role(c.role.Role())
	switch {
	case role == RoleMaster && resp.GetDiverged():
		c.log.Warn("peer diverged from us, re-pushing snapshot", "self", c.self, "peer_rev", resp.GetPeerRevision())
	case role == RoleBackup && resp.GetDiverged():
		c.log.Warn("we diverged from the master, awaiting PushConfig", "self", c.self, "our_rev", rev, "master_rev", resp.GetPeerRevision())
	}

	if role == RoleMaster {
		c.maybePush(ctx, hash, rev, resp.GetDiverged(), now)
	}
	// Both roles publish their health view: the MASTER needs the BACKUP's
	// view for and-consensus, and the BACKUP serves the MASTER's Diff/Status.
	if err := c.cli.PublishHealth(ctx, c.healthView()); err != nil {
		c.log.Warn("publish health failed", "self", c.self, "error", err)
	}
}

// selfIdentity returns the config hash and revision this node reports.
// The MASTER reads the store; the BACKUP reports its last applied snapshot
// (it must not touch git).
func (c *Controller) selfIdentity(ctx context.Context) (hash, rev string) {
	if Role(c.role.Role()) != RoleMaster {
		c.mu.Lock()
		defer c.mu.Unlock()
		if s := c.lastSnapshot; s != nil {
			return s.GetConfigHash(), s.GetRevision()
		}
		return "", ""
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
// the BACKUP sees the full desired picture.
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
	tables := make(map[string]string, len(st.Tables))
	for _, td := range st.Tables {
		tables[td.Name] = strings.Join(td.Members, "\n")
	}
	snap.Tables = tables
	if len(st.Anchors) > 0 {
		snap.Anchors = st.Anchors
	}
	return snap
}

// ApplySnapshot converges the local pf to a snapshot received from the
// MASTER (the M6 PeeringService server calls this on the BACKUP only).
func (c *Controller) ApplySnapshot(ctx context.Context, snap *pfkeeperv1.EffectiveConfigSnapshot) (reconcile.ReconcileResult, error) {
	st := &state.State{Anchors: map[string]string{}}
	for name, members := range snap.GetTables() {
		td := state.TableDef{Name: name}
		for _, line := range strings.Split(members, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				td.Members = append(td.Members, line)
			}
		}
		st.Tables = append(st.Tables, td)
	}
	for name, rules := range snap.GetAnchors() {
		st.Anchors[name] = rules
	}
	res, err := c.applier.Reconcile(ctx, st, snap.GetRevision())
	if err != nil {
		return res, err
	}
	c.mu.Lock()
	c.lastSnapshot = snap
	c.mu.Unlock()
	return res, nil
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
