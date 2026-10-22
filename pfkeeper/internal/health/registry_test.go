package health

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

func testSpec() state.HealthSpec {
	return state.HealthSpec{Type: "http", Port: 1, Path: "/h", Interval: "5s", Timeout: "1s", Rise: 1, Fall: 1}
}

func TestRegistryLifecycle(t *testing.T) {
	r := NewRegistry()

	if _, ok := r.Get("web", "10.0.0.1"); ok {
		t.Fatal("empty registry returned a monitor")
	}

	m1 := r.Register("web", "10.0.0.1", testSpec())
	m2 := r.Register("web", "10.0.0.2", testSpec())
	m3 := r.Register("db", "10.0.0.9", testSpec())

	list := r.List()
	if len(list) != 3 {
		t.Fatalf("List() = %d monitors, want 3", len(list))
	}
	// Ordered by (table, address).
	if list[0] != m3 || list[1] != m1 || list[2] != m2 {
		t.Fatal("List() is not sorted by (table, address)")
	}

	// Re-register keeps the monitor and refreshes the spec.
	spec := testSpec()
	spec.Rise = 5
	same := r.Register("web", "10.0.0.1", spec)
	if same != m1 {
		t.Fatal("re-register created a new monitor")
	}
	if m1.Spec.Rise != 5 {
		t.Fatalf("spec not refreshed: %+v", m1.Spec)
	}

	if !r.Remove("web", "10.0.0.2") {
		t.Fatal("Remove() reported false for an existing monitor")
	}
	if r.Remove("web", "10.0.0.2") {
		t.Fatal("Remove() reported true for a missing monitor")
	}
	if len(r.List()) != 2 {
		t.Fatalf("List() after remove = %d, want 2", len(r.List()))
	}
}

func TestRegistryView(t *testing.T) {
	r := NewRegistry()
	m := r.Register("web", "10.0.0.1", testSpec())
	m.Observe(Result{Passed: true, CheckedAt: time.Now()})
	m.Observe(Result{Passed: true, CheckedAt: time.Now()})

	view := r.View()
	if view["web/10.0.0.1"] != StateUP {
		t.Fatalf("view = %v, want web/10.0.0.1 UP", view)
	}
}

func TestRegistryForceCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	addr := srv.Listener.Addr().(*net.TCPAddr)

	r := NewRegistry()
	spec := testSpec()
	spec.Type, spec.Port, spec.Path = "http", addr.Port, "/h"
	m := r.Register("web", "127.0.0.1", spec)

	res, err := r.ForceCheck(context.Background(), "web", "127.0.0.1")
	if err != nil {
		t.Fatalf("ForceCheck() error = %v", err)
	}
	if !res.Passed || res.StatusCode != 200 {
		t.Fatalf("res = %+v, want pass 200", res)
	}
	if m.State() != StateUP {
		t.Fatalf("monitor state = %q, want UP", m.State())
	}

	if _, err := r.ForceCheck(context.Background(), "web", "10.9.9.9"); err == nil {
		t.Fatal("ForceCheck() of unknown member: error = nil, want error")
	}
}
