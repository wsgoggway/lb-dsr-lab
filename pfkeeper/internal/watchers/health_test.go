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
