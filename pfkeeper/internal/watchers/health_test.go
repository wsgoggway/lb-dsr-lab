package watchers

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/health"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/metrics"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/pfctl"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/reconcile"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

// TestHealthWatcherPublishesOnStart: the first probe pass publishes the FULL
// local view even without state changes — the peer view must never stay
// empty while members exist (t08 regression).
func TestHealthWatcherPublishesOnStart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	addr := srv.Listener.Addr().(*net.TCPAddr)

	st := &state.State{
		Tables: []state.TableDef{{
			Name:    "web",
			Members: []string{"127.0.0.1"},
			Health: &state.HealthSpec{
				Type: "http", Port: addr.Port, Path: "/", Interval: "5s", Timeout: "1s", Rise: 1, Fall: 1,
			},
		}},
		Anchors: map[string]string{},
	}

	var mu sync.Mutex
	var published []map[string]string
	w := &HealthWatcher{
		Reg:     health.NewRegistry(),
		Hc:      mustHC(t),
		Metrics: metrics.New("test"),
		State:   func() *state.State { return st },
		LocalSink: func(view map[string]string) {
			mu.Lock()
			defer mu.Unlock()
			published = append(published, view)
		},
	}

	if !w.ProbeOnce(context.Background()) {
		t.Fatal("first pass reported no change, want INACTIVE->UP")
	}
	mu.Lock()
	n := len(published)
	var got map[string]string
	if n > 0 {
		got = published[0]
	}
	mu.Unlock()
	if n == 0 {
		t.Fatal("LocalSink was not called on the first pass")
	}
	if got["web/127.0.0.1"] != health.StateUP {
		t.Fatalf("published view = %v, want web/127.0.0.1 UP", got)
	}

	// A second unchanged pass within the republish interval does not spam.
	w.ProbeOnce(context.Background())
	mu.Lock()
	n = len(published)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("publishes after unchanged pass = %d, want 1 (throttled)", n)
	}
}

func mustHC(t *testing.T) *reconcile.HealthController {
	t.Helper()
	hc, err := reconcile.NewHealthController(
		pfctl.NewClient("/sbin/pfctl", &stubPfRunner{}), nil, "and")
	if err != nil {
		t.Fatal(err)
	}
	return hc
}

// stubPfRunner answers pfctl calls in-process (table "web" already has both
// members; no real binary needed).
type stubPfRunner struct{}

func (stubPfRunner) Run(_ context.Context, _ string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "-t web -T show") {
		return "127.0.0.1\n", nil
	}
	return "", nil
}

// Loopback stand-ins for the lab members: the probers dial real addresses,
// so the unit test uses 127.0.0.x aliases (A: closed port -> DOWN, B: live
// endpoint -> UP).
const (
	addrDown = "127.0.0.2"
	addrUp   = "127.0.0.1"
)

// recordingPfRunner answers pfctl calls and records mutating argv lines.
type recordingPfRunner struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingPfRunner) Run(_ context.Context, _ string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	r.mu.Lock()
	r.calls = append(r.calls, joined)
	r.mu.Unlock()
	if strings.Contains(joined, "-t web -T show") {
		return addrDown + "\n" + addrUp + "\n", nil
	}
	return "", nil
}

func (r *recordingPfRunner) count(substr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

// TestApplyEffectiveChainKeys (t08 exact reproduction): local AND peer views
// agree (member A DOWN connect, member B UP 200) — the effective set must be
// {B} and only A is deleted. The views arrive in the composite "table/addr"
// key format actually used by Registry.View/SetPeerView; the watcher must
// narrow them per table before ApplyEffective, otherwise every member looks
// DOWN and the table empties (regression guard for the key-mismatch bug).
func TestApplyEffectiveChainKeys(t *testing.T) {
	ctx := context.Background()

	// Member A: closed loopback port (connect failure); member B: live http
	// endpoint. (The probers dial the real member addresses, so the unit
	// test uses loopback aliases instead of the lab IPs.)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	srvPort := srv.Listener.Addr().(*net.TCPAddr).Port

	addresses := []string{addrDown, addrUp}

	downSpec := state.HealthSpec{
		Type: "http", Port: closedPort, Path: "/", Interval: "5s", Timeout: "1s", Rise: 1, Fall: 1,
	}
	upSpec := state.HealthSpec{
		Type: "http", Port: srvPort, Path: "/", Interval: "5s", Timeout: "1s", Rise: 1, Fall: 1,
	}

	reg := health.NewRegistry()
	reg.Register("web", addrDown, downSpec)
	reg.Register("web", addrUp, upSpec)
	st := &state.State{
		Tables: []state.TableDef{{
			Name: "web", Members: addresses,
			Health: &upSpec,
		}},
		Anchors: map[string]string{},
	}

	runner := &recordingPfRunner{}
	pf := pfctl.NewClient("/sbin/pfctl", runner)
	hc, err := reconcile.NewHealthController(pf, nil, "and")
	if err != nil {
		t.Fatal(err)
	}

	// The peer view mirrors the local one, in the composite key format that
	// SetPeerView/PublishHealth actually produce.
	peerView := func() (map[string]string, bool) {
		return map[string]string{
			"web/" + addrDown: health.StateDOWN,
			"web/" + addrUp:   health.StateUP,
		}, true
	}

	w := &HealthWatcher{
		Reg:      reg,
		Hc:       hc,
		Metrics:  metrics.New("test"),
		State:    func() *state.State { return st },
		PeerView: peerView,
	}
	if !w.ProbeOnce(ctx) {
		t.Fatal("first pass reported no change, want A INACTIVE->DOWN")
	}

	if n := runner.count("-T delete " + addrDown); n != 1 {
		t.Fatalf("deletes of DOWN member = %d, want 1", n)
	}
	if n := runner.count("-T delete " + addrUp); n != 0 {
		t.Fatalf("deletes of healthy member = %d, want 0 (t08: table was emptied)", n)
	}
	if n := runner.count("-T add"); n != 0 {
		t.Fatalf("adds = %d, want 0 (both members already live)", n)
	}
}
