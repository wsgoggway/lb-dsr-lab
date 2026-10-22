package health

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

func TestMatchStatus(t *testing.T) {
	tests := []struct {
		name  string
		codes []string
		code  int
		want  bool
	}{
		{"empty list defaults to 2xx", nil, 200, true},
		{"empty list rejects 500", nil, 500, false},
		{"exact match", []string{"200"}, 200, true},
		{"exact mismatch", []string{"200"}, 201, false},
		{"range hit", []string{"200-399"}, 302, true},
		{"range miss", []string{"200-399"}, 404, false},
		{"class hit upper", []string{"2XX"}, 299, true},
		{"class hit lower", []string{"2xx"}, 200, true},
		{"class miss", []string{"2xx"}, 500, false},
		{"mixed list", []string{"200", "2xx", "404"}, 404, true},
		{"mixed list miss", []string{"200", "404"}, 500, false},
		{"invalid entry ignored", []string{"bogus", "200"}, 200, true},
		{"only invalid entry", []string{"bogus"}, 200, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MatchStatus(tt.codes, tt.code); got != tt.want {
				t.Fatalf("MatchStatus(%v, %d) = %v, want %v", tt.codes, tt.code, got, tt.want)
			}
		})
	}
}

func TestHTTPProber(t *testing.T) {
	okJSON := []byte(`{"status":"ok","version":2}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(okJSON)
		case "/degraded":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"fail"}`))
		case "/broken":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{not json`))
		case "/err":
			http.Error(w, "oops", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	hostOnly := addr[:strings.LastIndex(addr, ":")]

	spec := func(status []string, jq string) state.HealthSpec {
		return state.HealthSpec{
			Type: "http", Port: portOf(addr), Path: "/healthz",
			Interval: "5s", Timeout: "2s", Rise: 1, Fall: 1,
			Expect: state.HealthExpect{Status: status, JQ: jq},
		}
	}
	prober := HTTPProber{Timeout: 2 * time.Second}

	t.Run("200 without jq passes", func(t *testing.T) {
		res := prober.Probe(context.Background(), hostOnly, spec(nil, ""))
		if !res.Passed || res.Reason != "" || res.StatusCode != 200 {
			t.Fatalf("res = %+v, want pass 200", res)
		}
		if res.State != StateUP || res.CheckedAt.IsZero() || res.Duration <= 0 {
			t.Fatalf("res metadata wrong: %+v", res)
		}
		if res.BodyExcerpt != string(okJSON) {
			t.Fatalf("BodyExcerpt = %q", res.BodyExcerpt)
		}
	})
	t.Run("200 plus jq true passes", func(t *testing.T) {
		res := prober.Probe(context.Background(), hostOnly, spec(nil, `.status == "ok"`))
		if !res.Passed || res.JQOutput != "true" {
			t.Fatalf("res = %+v, want pass with JQOutput true", res)
		}
	})
	t.Run("200 plus jq false yields jq_false", func(t *testing.T) {
		s := spec(nil, `.status == "ok"`)
		s.Path = "/degraded"
		res := prober.Probe(context.Background(), hostOnly, s)
		if res.Passed || res.Reason != ReasonJQFalse {
			t.Fatalf("res = %+v, want jq_false", res)
		}
	})
	t.Run("jq nil result yields jq_false", func(t *testing.T) {
		res := prober.Probe(context.Background(), hostOnly, spec(nil, `.missing // empty`))
		if res.Passed || res.Reason != ReasonJQFalse || res.JQOutput != "" {
			t.Fatalf("res = %+v, want jq_false without output", res)
		}
	})
	t.Run("invalid json plus jq yields json_invalid", func(t *testing.T) {
		s := spec(nil, `.status == "ok"`)
		s.Path = "/broken"
		res := prober.Probe(context.Background(), hostOnly, s)
		if res.Passed || res.Reason != ReasonJSONInvald {
			t.Fatalf("res = %+v, want json_invalid", res)
		}
	})
	t.Run("broken jq expression yields jq_error", func(t *testing.T) {
		res := prober.Probe(context.Background(), hostOnly, spec(nil, `.status ==`))
		if res.Passed || res.Reason != ReasonJQError {
			t.Fatalf("res = %+v, want jq_error", res)
		}
	})
	t.Run("500 yields status_code with body kept", func(t *testing.T) {
		s := spec(nil, "")
		s.Path = "/err"
		res := prober.Probe(context.Background(), hostOnly, s)
		if res.Passed || res.Reason != ReasonStatus || res.StatusCode != 500 {
			t.Fatalf("res = %+v, want status_code 500", res)
		}
		if !strings.Contains(res.BodyExcerpt, "oops") {
			t.Fatalf("BodyExcerpt = %q, want body kept for inspect", res.BodyExcerpt)
		}
	})
	t.Run("status range accepts 200-299", func(t *testing.T) {
		res := prober.Probe(context.Background(), hostOnly, spec([]string{"200-299"}, ""))
		if !res.Passed {
			t.Fatalf("res = %+v, want pass", res)
		}
	})
	t.Run("timeout on slow server", func(t *testing.T) {
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
		}))
		defer slow.Close()
		slowAddr := strings.TrimPrefix(slow.URL, "http://")
		slowHost := slowAddr[:strings.LastIndex(slowAddr, ":")]
		s := spec(nil, "")
		s.Port = portOf(slowAddr)
		res := (HTTPProber{Timeout: 50 * time.Millisecond}).Probe(context.Background(), slowHost, s)
		if res.Passed || res.Reason != ReasonTimeout {
			t.Fatalf("res = %+v, want timeout", res)
		}
	})
	t.Run("connection refused yields connect", func(t *testing.T) {
		// Reserve a port, then close it: connections to it get refused.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		closedPort := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		s := spec(nil, "")
		s.Port = closedPort
		res := prober.Probe(context.Background(), "127.0.0.1", s)
		if res.Passed || res.Reason != ReasonConnect {
			t.Fatalf("res = %+v, want connect", res)
		}
	})
}

func TestTCPProber(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	host := addr[:strings.LastIndex(addr, ":")]

	spec := state.HealthSpec{Type: "tcp", Port: portOf(addr), Timeout: "1s", Interval: "5s", Rise: 1, Fall: 1}
	if res := (TCPProber{}).Probe(context.Background(), host, spec); !res.Passed || res.Reason != "" {
		t.Fatalf("TCP res = %+v, want pass", res)
	}
	spec.Port = 1 // closed port
	res := (TCPProber{}).Probe(context.Background(), "127.0.0.1", spec)
	if res.Passed || res.Reason != ReasonConnect {
		t.Fatalf("TCP res = %+v, want connect", res)
	}
}

func TestPick(t *testing.T) {
	for _, typ := range []string{"http", "tcp", "icmp"} {
		if Pick(state.HealthSpec{Type: typ}) == nil {
			t.Fatalf("Pick(%s) = nil", typ)
		}
	}
	if Pick(state.HealthSpec{Type: "grpc"}) != nil {
		t.Fatal("Pick(unknown) != nil")
	}
}

// portOf extracts the numeric port from "host:port".
func portOf(addr string) int {
	idx := strings.LastIndex(addr, ":")
	n := 0
	for _, c := range addr[idx+1:] {
		n = n*10 + int(c-'0')
	}
	return n
}
