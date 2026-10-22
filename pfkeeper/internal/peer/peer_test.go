package peer

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	pfkeeperv1 "github.com/wsgoggway/lb-dsr-lab/pfkeeper/api/pfkeeperv1"
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

func (f *fakeApplier) Reconcile(_ context.Context, st *state.State, rev string) (reconcile.ReconcileResult, error) {
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
	return &pfkeeperv1.SyncCheckResponse{Diverged: f.diverged, PeerRevision: "peerrev1", PeerConfigHash: "hash1"}, nil
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
