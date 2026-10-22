package peer

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	pfkeeperv1 "github.com/wsgoggway/lb-dsr-lab/pfkeeper/api/pfkeeperv1"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/pfctl"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/reconcile"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

// fakeApplier records Reconcile invocations (stands in for the config
// reconciler).
type fakeApplier struct {
	mu        sync.Mutex
	calls     int
	lastState *state.State
	lastRev   string
	err       error
}

func (f *fakeApplier) Reconcile(_ context.Context, st *state.State, rev string, _ reconcile.HealthyView) (reconcile.ReconcileResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastState, f.lastRev = st, rev
	return reconcile.ReconcileResult{Revision: rev}, f.err
}

func (f *fakeApplier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakePeer is an in-memory PeeringServiceServer with programmable behaviour.
type fakePeer struct {
	pfkeeperv1.UnimplementedPeeringServiceServer

	mu         sync.Mutex
	diverged   bool
	fail       bool
	masterHash string // when set, diverged = req hash != masterHash
	masterRev  string
	pushes     []*pfkeeperv1.EffectiveConfigSnapshot
	healths    []*pfkeeperv1.HealthView
	syncChecks []*pfkeeperv1.SyncCheckRequest
	redirects  []*pfkeeperv1.RedirectWriteRequest
}

func (f *fakePeer) setDiverged(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.diverged = v
}

func (f *fakePeer) setFail(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = v
}

func (f *fakePeer) snapshotState() (int, int, int, []*pfkeeperv1.SyncCheckRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pushes), len(f.healths), len(f.redirects), f.syncChecks
}

func (f *fakePeer) PushConfig(_ context.Context, snap *pfkeeperv1.EffectiveConfigSnapshot) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errors.New("peer down")
	}
	f.pushes = append(f.pushes, snap)
	return &emptypb.Empty{}, nil
}

func (f *fakePeer) PublishHealth(_ context.Context, view *pfkeeperv1.HealthView) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errors.New("peer down")
	}
	f.healths = append(f.healths, view)
	return &emptypb.Empty{}, nil
}

func (f *fakePeer) SyncCheck(_ context.Context, req *pfkeeperv1.SyncCheckRequest) (*pfkeeperv1.SyncCheckResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errors.New("peer down")
	}
	f.syncChecks = append(f.syncChecks, req)
	diverged := f.diverged
	if f.masterHash != "" {
		diverged = req.GetConfigHash() != f.masterHash
	}
	return &pfkeeperv1.SyncCheckResponse{Diverged: diverged, PeerRevision: "peerrev1", PeerConfigHash: f.masterHash}, nil
}

func (f *fakePeer) RedirectWrite(_ context.Context, req *pfkeeperv1.RedirectWriteRequest) (*pfkeeperv1.RedirectWriteResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errors.New("peer down")
	}
	f.redirects = append(f.redirects, req)
	return &pfkeeperv1.RedirectWriteResponse{Payload: req.GetPayload()}, nil
}

// captureHandler collects log record messages for assertions.
type captureHandler struct {
	mu      sync.Mutex
	records []string
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Message)
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }
func (h *captureHandler) count(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.records {
		if m == msg {
			n++
		}
	}
	return n
}

// startPeer serves fakePeer over bufconn and returns a client wired to it.
func startPeer(t *testing.T, fp *fakePeer) *PeerClient {
	t.Helper()
	ln := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	pfkeeperv1.RegisterPeeringServiceServer(srv, fp)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return &PeerClient{
		Endpoint: "passthrough:///bufnet",
		NodeID:   "lb1",
		PeerID:   "lb2",
		Dialer: func(ctx context.Context, _ string) (net.Conn, error) {
			return ln.DialContext(ctx)
		},
	}
}

// seedStore writes a small state tree (no git — the controller tolerates a
// missing HEAD and hashes files directly).
func seedStore(t *testing.T) *state.Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tables"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("sync:\n  mode: and\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tables", "web.yaml"), []byte("name: web\nmembers:\n  - 10.0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &state.Store{Dir: dir, Branch: "main"}
}

func newController(t *testing.T, role string, cli *PeerClient, store *state.Store, applier ConfigApplier, logger *slog.Logger) *Controller {
	t.Helper()
	c := NewController(NewStaticRole(role), "lb1", "lb2:8444", cli, applier, nil, store)
	if logger != nil {
		c.SetLogger(logger)
	}
	return c
}

func TestMasterPushesOnChange(t *testing.T) {
	ctx := context.Background()
	fp := &fakePeer{}
	cli := startPeer(t, fp)
	defer cli.Close()
	applier := &fakeApplier{}
	c := newController(t, "MASTER", cli, seedStore(t), applier, nil)

	// First tick: initial push (nothing pushed yet).
	c.tickOnce(ctx)
	pushes, healths, _, _ := fp.snapshotState()
	if pushes != 1 {
		t.Fatalf("pushes after first tick = %d, want 1", pushes)
	}
	if healths != 1 {
		t.Fatalf("healths after first tick = %d, want 1", healths)
	}

	// Second tick without changes: no new push, health still published.
	c.tickOnce(ctx)
	pushes, healths, _, _ = fp.snapshotState()
	if pushes != 1 {
		t.Fatalf("pushes after unchanged tick = %d, want 1", pushes)
	}
	if healths != 2 {
		t.Fatalf("healths after second tick = %d, want 2", healths)
	}

	// State change (hash changes) -> push again with fresh members.
	store := c.store
	if err := os.WriteFile(filepath.Join(store.Dir, "tables", "web.yaml"),
		[]byte("name: web\nmembers:\n  - 10.0.0.1\n  - 10.0.0.2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.tickOnce(ctx)
	pushes, _, _, _ = fp.snapshotState()
	if pushes != 2 {
		t.Fatalf("pushes after change = %d, want 2", pushes)
	}
	snap := fp.pushes[1]
	if got := snap.GetTables()["web"]; got != "10.0.0.1\n10.0.0.2" {
		t.Fatalf("snapshot web members = %q, want both members", got)
	}
	if snap.GetConfigHash() == "" {
		t.Fatal("snapshot config hash is empty")
	}
}

func TestMasterRePushesOnDivergence(t *testing.T) {
	ctx := context.Background()
	fp := &fakePeer{}
	cli := startPeer(t, fp)
	defer cli.Close()
	c := newController(t, "MASTER", cli, seedStore(t), &fakeApplier{}, nil)

	c.tickOnce(ctx) // initial push
	c.tickOnce(ctx) // no change, no push
	pushes, _, _, _ := fp.snapshotState()
	if pushes != 1 {
		t.Fatalf("pushes before divergence = %d, want 1", pushes)
	}

	fp.setDiverged(true)
	c.tickOnce(ctx) // divergence guard -> heal push
	pushes, _, _, _ = fp.snapshotState()
	if pushes != 2 {
		t.Fatalf("pushes after divergence = %d, want 2", pushes)
	}
	if st := c.PeerStatus(); !st.Diverged || !st.Reachable {
		t.Fatalf("peer status = %+v, want reachable+diverged", st)
	}
}

func TestBackupAppliesSnapshotAndPublishes(t *testing.T) {
	ctx := context.Background()
	fp := &fakePeer{}
	cli := startPeer(t, fp)
	defer cli.Close()
	applier := &fakeApplier{}
	c := newController(t, "BACKUP", cli, seedStore(t), applier, nil)

	snap := &pfkeeperv1.EffectiveConfigSnapshot{
		Revision: "deadbee",
		Tables:   map[string]string{"web": "10.0.0.1\n10.0.0.2"},
		Anchors:  map[string]string{"web": "pass in quick to <web>\n"},
	}
	if _, err := c.ApplySnapshot(ctx, snap); err != nil {
		t.Fatalf("ApplySnapshot() error = %v", err)
	}
	if applier.count() != 1 {
		t.Fatalf("applier calls = %d, want 1", applier.count())
	}
	if applier.lastRev != "deadbee" {
		t.Fatalf("applier revision = %q, want deadbee", applier.lastRev)
	}
	if len(applier.lastState.Tables) != 1 || applier.lastState.Tables[0].Name != "web" ||
		len(applier.lastState.Tables[0].Members) != 2 {
		t.Fatalf("applier state = %+v", applier.lastState)
	}
	if applier.lastState.Anchors["web"] != "pass in quick to <web>\n" {
		t.Fatalf("applier anchors = %v", applier.lastState.Anchors)
	}
	if c.LastSnapshot() != snap {
		t.Fatal("last snapshot not cached")
	}

	// BACKUP ticks: publishes health, never pushes, reports the applied
	// snapshot revision in SyncCheck.
	c.SetLocalView(map[string]string{LocalViewKey("web", "10.0.0.1"): "UP"})
	c.tickOnce(ctx)
	pushes, healths, _, checks := fp.snapshotState()
	if pushes != 0 {
		t.Fatalf("BACKUP pushed %d snapshots, want 0", pushes)
	}
	if healths != 1 {
		t.Fatalf("BACKUP healths = %d, want 1", healths)
	}
	if fp.healths[0].GetNode() != "lb1" || len(fp.healths[0].GetMembers()) != 1 ||
		fp.healths[0].GetMembers()[0].GetTable() != "web" ||
		fp.healths[0].GetMembers()[0].GetAddress() != "10.0.0.1" ||
		fp.healths[0].GetMembers()[0].GetState() != "UP" {
		t.Fatalf("health view = %+v", fp.healths[0])
	}
	if len(checks) != 1 || checks[0].GetRevision() != "deadbee" {
		t.Fatalf("sync checks = %+v, want revision deadbee", checks)
	}
}

func TestDegradedPeerNoPanicAndThrottledWarn(t *testing.T) {
	ctx := context.Background()
	fp := &fakePeer{}
	// A client whose dial always fails: no listener behind it.
	cli := &PeerClient{
		Endpoint: "passthrough:///bufnet",
		NodeID:   "lb1",
		PeerID:   "lb2",
		Dialer: func(context.Context, string) (net.Conn, error) {
			return nil, errors.New("connection refused")
		},
	}
	defer cli.Close()
	capture := &captureHandler{}
	c := newController(t, "BACKUP", cli, seedStore(t), &fakeApplier{}, slog.New(capture))

	c.tickOnce(ctx)
	if st := c.PeerStatus(); st.Reachable || !st.Degraded {
		t.Fatalf("status = %+v, want unreachable+degraded", st)
	}
	c.tickOnce(ctx) // within the warn interval: no second warning

	if capture.count("peer unreachable, running degraded") != 1 {
		t.Fatalf("degraded warnings = %d, want 1 (throttled)", capture.count("peer unreachable, running degraded"))
	}
	pushes, _, _, _ := fp.snapshotState()
	if pushes != 0 {
		t.Fatalf("degraded node pushed %d snapshots, want 0", pushes)
	}
}

func TestProxyToMaster(t *testing.T) {
	ctx := context.Background()
	fp := &fakePeer{}
	cli := startPeer(t, fp)
	defer cli.Close()

	payload := []byte(`{"table":"web"}`)
	got, err := cli.ProxyToMaster(ctx, "TableService/SetTable", payload, "operator")
	if err != nil {
		t.Fatalf("ProxyToMaster() error = %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("payload echo = %q, want %q", got, payload)
	}
	_, _, nRedirects, _ := fp.snapshotState()
	if nRedirects != 1 {
		t.Fatalf("redirect count = %d, want 1", nRedirects)
	}
	fp.mu.Lock()
	redirects := fp.redirects
	fp.mu.Unlock()
	if redirects[0].GetMethod() != "TableService/SetTable" ||
		redirects[0].GetActorCn() != "operator" {
		t.Fatalf("redirects = %+v", redirects)
	}

	// Failure path surfaces as an error.
	fp.setFail(true)
	if _, err := cli.ProxyToMaster(ctx, "TableService/SetTable", payload, "operator"); err == nil {
		t.Fatal("ProxyToMaster() error = nil on failing peer, want error")
	}
}

func TestLocalViewKeyRoundTrip(t *testing.T) {
	k := LocalViewKey("web", "192.168.100.21")
	table, addr, ok := splitLocalViewKey(k)
	if !ok || table != "web" || addr != "192.168.100.21" {
		t.Fatalf("round trip of %q = (%q, %q, %v)", k, table, addr, ok)
	}
	if _, _, ok := splitLocalViewKey("noseparator"); ok {
		t.Fatal("splitLocalViewKey(noseparator) ok=true, want false")
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	fp := &fakePeer{}
	cli := startPeer(t, fp)
	defer cli.Close()
	c := newController(t, "MASTER", cli, seedStore(t), &fakeApplier{}, nil)
	c.TickInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

// TestSyncCheckIdentityUsesAppliedSnapshot: the BACKUP's SyncCheck identity
// is the applied snapshot, not the stale local git hash.
func TestSyncCheckIdentityUsesAppliedSnapshot(t *testing.T) {
	ctx := context.Background()
	fp := &fakePeer{masterHash: "masterhash", masterRev: "masterrev"}
	cli := startPeer(t, fp)
	defer cli.Close()
	applier := &fakeApplier{}

	// Scenario A: applied snapshot == master hash -> no divergence reported.
	cApplied := newController(t, "BACKUP", cli, seedStore(t), applier, nil)
	cApplied.SetAppliedSnapshot("masterhash", "masterrev")
	cApplied.SyncOnce(ctx)
	if st := cApplied.PeerStatus(); st.Diverged {
		t.Fatalf("BACKUP with matching applied snapshot: status = %+v, want not diverged", st)
	}
	fp.mu.Lock()
	checksA := len(fp.syncChecks)
	fp.mu.Unlock()
	if checksA != 1 || fp.syncChecks[0].GetConfigHash() != "masterhash" || fp.syncChecks[0].GetRevision() != "masterrev" {
		t.Fatalf("first SyncCheck request = %+v, want applied snapshot identity", fp.syncChecks[0])
	}

	// Scenario B: no applied snapshot yet -> the stale LOCAL git hash is
	// sent, the master answers diverged=true (its view differs).
	cStale := newController(t, "BACKUP", cli, seedStore(t), applier, nil)
	cStale.SyncOnce(ctx)
	if st := cStale.PeerStatus(); !st.Diverged {
		t.Fatalf("BACKUP without applied snapshot: status = %+v, want diverged", st)
	}
	fp.mu.Lock()
	last := fp.syncChecks[len(fp.syncChecks)-1]
	fp.mu.Unlock()
	if last.GetConfigHash() == "" || last.GetConfigHash() == "masterhash" {
		t.Fatalf("stale BACKUP sent hash %q, want the local git hash", last.GetConfigHash())
	}

	// Scenario C: MASTER whose SyncCheck reports divergence re-pushes
	// (heal), and RequestHeal forces a push even without divergence.
	// Legacy fake mode (explicit diverged flag): the master sends its own
	// store hash, which the fake must not compare against anything.
	fp2 := &fakePeer{}
	cli2 := startPeer(t, fp2)
	defer cli2.Close()
	cMaster := newController(t, "MASTER", cli2, seedStore(t), applier, nil)
	cMaster.SyncOnce(ctx) // initial push
	fp2.mu.Lock()
	pushes := len(fp2.pushes)
	fp2.mu.Unlock()
	if pushes != 1 {
		t.Fatalf("initial pushes = %d, want 1", pushes)
	}
	fp2.setDiverged(true)
	cMaster.SyncOnce(ctx) // heal push
	fp2.mu.Lock()
	pushes = len(fp2.pushes)
	fp2.mu.Unlock()
	if pushes != 2 {
		t.Fatalf("pushes after divergence = %d, want 2 (heal)", pushes)
	}
	fp2.setDiverged(false)
	cMaster.SyncOnce(ctx) // no change, no divergence -> no push
	fp2.mu.Lock()
	pushes = len(fp2.pushes)
	fp2.mu.Unlock()
	if pushes != 2 {
		t.Fatalf("pushes without divergence/change = %d, want 2", pushes)
	}
	cMaster.RequestHeal()
	cMaster.SyncOnce(ctx) // requested heal -> push
	fp2.mu.Lock()
	pushes = len(fp2.pushes)
	fp2.mu.Unlock()
	if pushes != 3 {
		t.Fatalf("pushes after RequestHeal = %d, want 3", pushes)
	}
}

// TestApplySnapshotRecordsIdentity: applying a snapshot records its hash/rev
// as the SyncCheck identity (server PushConfig hook does the same).
func TestApplySnapshotRecordsIdentity(t *testing.T) {
	ctx := context.Background()
	fp := &fakePeer{masterHash: "masterhash", masterRev: "masterrev"}
	cli := startPeer(t, fp)
	defer cli.Close()
	c := newController(t, "BACKUP", cli, seedStore(t), &fakeApplier{}, nil)

	if _, err := c.ApplySnapshot(ctx, &pfkeeperv1.EffectiveConfigSnapshot{
		Revision:   "masterrev",
		ConfigHash: "masterhash",
		Tables:     map[string]string{"web": "10.0.0.1"},
	}); err != nil {
		t.Fatalf("ApplySnapshot() error = %v", err)
	}
	c.SyncOnce(ctx)
	fp.mu.Lock()
	last := fp.syncChecks[len(fp.syncChecks)-1]
	fp.mu.Unlock()
	if last.GetConfigHash() != "masterhash" || last.GetRevision() != "masterrev" {
		t.Fatalf("identity after apply = (%q, %q), want (masterhash, masterrev)",
			last.GetConfigHash(), last.GetRevision())
	}
	if st := c.PeerStatus(); st.Diverged {
		t.Fatalf("status = %+v, want not diverged after heal", st)
	}
}

// simPf is a stateful pf simulator: TableShow returns the live member set,
// add/delete mutate it — so health passes observe their own effect.
type simPf struct {
	mu    sync.Mutex
	live  map[string]bool
	calls []string
}

func newSimPf(members ...string) *simPf {
	live := map[string]bool{}
	for _, m := range members {
		live[m] = true
	}
	return &simPf{live: live}
}

func (s *simPf) Run(_ context.Context, _ string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, joined)
	switch {
	case strings.Contains(joined, "-t web -T show"):
		live := make([]string, 0, len(s.live))
		for m := range s.live {
			live = append(live, m)
		}
		sort.Strings(live)
		return strings.Join(live, "\n"), nil
	case strings.Contains(joined, "-T add "):
		s.live[args[len(args)-1]] = true
	case strings.Contains(joined, "-T delete "):
		delete(s.live, args[len(args)-1])
	}
	return "", nil
}

func (s *simPf) has(member string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live[member]
}

func (s *simPf) count(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

// snapshotApplier models the BACKUP config layer: Reconcile lands the full
// desired set in pf (TableReplace verbatim).
type snapshotApplier struct {
	sim   *simPf
	calls int
}

func (a *snapshotApplier) Reconcile(_ context.Context, st *state.State, rev string, _ reconcile.HealthyView) (reconcile.ReconcileResult, error) {
	a.calls++
	for _, td := range st.Tables {
		for _, m := range td.Members {
			a.sim.Run(context.Background(), "pfctl", "-t", td.Name, "-T", "add", m) //nolint:errcheck // simulator
		}
	}
	return reconcile.ReconcileResult{Revision: rev}, nil
}

// TestBackupSnapshotPrunedByLocalHealth (t08 BACKUP regression): a snapshot
// lands as full desired, and the health layer must prune it to
// desired ∩ localHealthy IMMEDIATELY — and again on every repeat snapshot,
// without ever touching the healthy member.
func TestBackupSnapshotPrunedByLocalHealth(t *testing.T) {
	ctx := context.Background()
	const mDown = "192.168.100.21"
	const mUp = "192.168.100.22"

	sim := newSimPf()
	cli := startPeer(t, &fakePeer{})
	defer cli.Close()

	hc, err := reconcile.NewHealthController(pfctl.NewClient("/sbin/pfctl", sim), nil, "and")
	if err != nil {
		t.Fatal(err)
	}
	c := NewController(NewStaticRole(string(RoleBackup)), "lb2", "lb1:8444", cli,
		&snapshotApplier{sim: sim}, hc, seedStore(t))

	// Local health: .21 DOWN (rs1 nginx stopped), .22 UP.
	c.SetLocalView(map[string]string{
		LocalViewKey("web", mDown): "DOWN",
		LocalViewKey("web", mUp):   "UP",
	})

	snap := &pfkeeperv1.EffectiveConfigSnapshot{
		Revision:   "r1",
		ConfigHash: "h1",
		Tables:     map[string]string{"web": mDown + "\n" + mUp},
	}

	// First snapshot: full desired lands, health prunes .21 at once.
	if _, err := c.ApplySnapshot(ctx, snap); err != nil {
		t.Fatalf("ApplySnapshot() error = %v", err)
	}
	if sim.has(mDown) {
		t.Fatal("health-excluded .21 still live after snapshot")
	}
	if !sim.has(mUp) {
		t.Fatal("healthy .22 missing after snapshot")
	}
	if n := sim.count("-T delete " + mUp); n != 0 {
		t.Fatalf("healthy member deleted %d times, want 0", n)
	}

	// Periodic repeat snapshot (same desired): prune runs again, .21 does
	// not survive, .22 still never touched.
	if _, err := c.ApplySnapshot(ctx, snap); err != nil {
		t.Fatalf("second ApplySnapshot() error = %v", err)
	}
	if sim.has(mDown) || !sim.has(mUp) {
		t.Fatalf("pf = %v/%v, want only the healthy member", sim.has(mDown), sim.has(mUp))
	}
	if n := sim.count("-T delete " + mUp); n != 0 {
		t.Fatalf("healthy member deleted %d times after repeat, want 0", n)
	}
	if n := sim.count("-T delete " + mDown); n < 2 {
		t.Fatalf("DOWN member deleted %d times, want >=2 (once per snapshot)", n)
	}
}
