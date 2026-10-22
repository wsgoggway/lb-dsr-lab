package state

import (
	"os"
	"path/filepath"
	"testing"
)

// writeStateTree creates a full state directory under t.TempDir().
func writeStateTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("config.yaml", `sync:
  mode: and
  health_consensus: quorum
  lag_alert: 30s
  auto_heal: true
`)
	mustWrite("tables/web.yaml", `name: web
persist: true
members:
  - 192.168.100.21
  - 192.168.100.22
description: lab backends
health:
  type: http
  port: 8080
  path: /healthz
  interval: 5s
  timeout: 2s
  rise: 2
  fall: 3
  expect:
    status:
      - "200-299"
    jq: '.status == "ok"'
`)
	mustWrite("rules/web.conf", "pass in quick proto tcp to <web> port 80\n")
	mustWrite("conf/pf.conf", "set skip on lo0\n")
	return dir
}

func TestLoadStateFullTree(t *testing.T) {
	dir := writeStateTree(t)
	st, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if st.Config.Sync.Mode != "and" || st.Config.Sync.LagAlert != "30s" {
		t.Fatalf("Config = %+v", st.Config.Sync)
	}
	if st.Config.Sync.AutoHeal == nil || !*st.Config.Sync.AutoHeal {
		t.Fatalf("AutoHeal = %v, want true", st.Config.Sync.AutoHeal)
	}
	if len(st.Tables) != 1 {
		t.Fatalf("got %d tables, want 1", len(st.Tables))
	}
	td := st.Tables[0]
	if td.Name != "web" || !td.Persist || len(td.Members) != 2 || td.Members[0] != "192.168.100.21" {
		t.Fatalf("TableDef = %+v", td)
	}
	if td.Health == nil || td.Health.Type != "http" || td.Health.Port != 8080 || td.Health.Rise != 2 || td.Health.Fall != 3 {
		t.Fatalf("Health = %+v", td.Health)
	}
	if td.Health.Expect.JQ != `.status == "ok"` {
		t.Fatalf("Expect.JQ = %q", td.Health.Expect.JQ)
	}
	if st.Anchors["web"] != "pass in quick proto tcp to <web> port 80\n" {
		t.Fatalf("Anchors = %v", st.Anchors)
	}
	if st.MainConf != "set skip on lo0\n" {
		t.Fatalf("MainConf = %q", st.MainConf)
	}
}

func TestLoadStateEmptyDir(t *testing.T) {
	st, err := LoadState(t.TempDir())
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if len(st.Tables) != 0 || len(st.Anchors) != 0 || st.MainConf != "" {
		t.Fatalf("state = %+v, want zero values", st)
	}
}

func TestLoadStateBrokenYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("sync: [unclosed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(dir); err == nil {
		t.Fatal("LoadState() error = nil, want parse error")
	}
}

func TestValidateState(t *testing.T) {
	valid := State{Tables: []TableDef{{
		Name:    "web",
		Members: []string{"192.168.100.21", "192.168.100.22"},
		Health: &HealthSpec{
			Type: "http", Port: 8080, Interval: "5s", Timeout: "2s",
			Rise: 2, Fall: 3,
			Expect: HealthExpect{Status: []string{"200-299", "2xx"}, JQ: ".status == \"ok\""},
		},
	}}}
	if err := ValidateState(valid); err != nil {
		t.Fatalf("ValidateState(valid) error = %v", err)
	}

	tests := []struct {
		name  string
		mutFn func(*State)
	}{
		{"bad table name", func(s *State) { s.Tables[0].Name = "1bad" }},
		{"table name too long", func(s *State) { s.Tables[0].Name = "abcdefghijklmnopqrstuvwxzy0123456" }},
		{"ipv6 member rejected in v1", func(s *State) { s.Tables[0].Members[0] = "fd00::1" }},
		{"not an ip", func(s *State) { s.Tables[0].Members[0] = "backend1" }},
		{"duplicate member", func(s *State) { s.Tables[0].Members[1] = "192.168.100.21" }},
		{"duplicate table", func(s *State) {
			s.Tables = append(s.Tables, s.Tables[0])
		}},
		{"bad health type", func(s *State) { s.Tables[0].Health.Type = "grpc" }},
		{"http without port", func(s *State) { s.Tables[0].Health.Port = 0 }},
		{"bad interval", func(s *State) { s.Tables[0].Health.Interval = "five" }},
		{"zero interval", func(s *State) { s.Tables[0].Health.Interval = "0s" }},
		{"zero rise", func(s *State) { s.Tables[0].Health.Rise = 0 }},
		{"zero fall", func(s *State) { s.Tables[0].Health.Fall = 0 }},
		{"bad status entry", func(s *State) { s.Tables[0].Health.Expect.Status = []string{"2OO"} }},
		{"broken jq", func(s *State) { s.Tables[0].Health.Expect.JQ = ".status ==" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := cloneState(valid)
			tt.mutFn(&s)
			if err := ValidateState(s); err == nil {
				t.Fatalf("ValidateState() error = nil, want error")
			}
		})
	}
}

// cloneState deep-copies a State so table-driven mutations stay isolated.
func cloneState(s State) State {
	out := State{Config: s.Config, Anchors: s.Anchors, MainConf: s.MainConf}
	out.Tables = append([]TableDef(nil), s.Tables...)
	for i := range out.Tables {
		out.Tables[i].Members = append([]string(nil), out.Tables[i].Members...)
		if h := out.Tables[i].Health; h != nil {
			cp := *h
			cp.Expect.Status = append([]string(nil), h.Expect.Status...)
			out.Tables[i].Health = &cp
		}
	}
	return out
}

// tcpAndIcmpNeedNoPort checks that tcp health without a port is valid.
func TestTCPHealthWithoutPortIsValid(t *testing.T) {
	st := State{Tables: []TableDef{{
		Name:    "web",
		Members: []string{"192.168.100.21"},
		Health:  &HealthSpec{Type: "icmp", Interval: "10s", Timeout: "2s", Rise: 1, Fall: 1},
	}}}
	if err := ValidateState(st); err != nil {
		t.Fatalf("ValidateState(icmp, no port) error = %v", err)
	}
}
