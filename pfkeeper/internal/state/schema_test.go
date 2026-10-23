package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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
	if td.Name != "web" || !td.Persist || len(td.Members) != 2 || td.Members[0].Addr != "192.168.100.21" || td.Members[0].Weight != DefaultMemberWeight {
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
		Members: PlainMembers("192.168.100.21", "192.168.100.22"),
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
		{"ipv6 member rejected in v1", func(s *State) { s.Tables[0].Members[0].Addr = "fd00::1" }},
		{"not an ip", func(s *State) { s.Tables[0].Members[0].Addr = "backend1" }},
		{"duplicate member", func(s *State) { s.Tables[0].Members[1].Addr = "192.168.100.21" }},
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
		out.Tables[i].Members = append([]MemberDef(nil), out.Tables[i].Members...)
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
		Members: PlainMembers("192.168.100.21"),
		Health:  &HealthSpec{Type: "icmp", Interval: "10s", Timeout: "2s", Rise: 1, Fall: 1},
	}}}
	if err := ValidateState(st); err != nil {
		t.Fatalf("ValidateState(icmp, no port) error = %v", err)
	}
}

// TestLoadStateBodyJQFolded (live-stand regression): the expect block may
// carry the nested body.jq shape; it must fold into Expect.JQ, otherwise the
// prober silently skips the jq check and dead members stay UP.
func TestLoadStateBodyJQFolded(t *testing.T) {
	dir := t.TempDir()
	yamlBody := `name: webs
members:
  - 192.168.100.21
health:
  type: http
  port: 80
  path: /healthz
  interval: 5s
  timeout: 2s
  rise: 2
  fall: 3
  expect:
    status:
      - 2xx
    body:
      jq: '.status == "ok"'
    jq: ""
`
	if err := os.MkdirAll(filepath.Join(dir, "tables"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tables", "webs.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	td := st.Tables[0]
	if td.Health == nil || td.Health.Expect.JQ != `.status == "ok"` {
		t.Fatalf("Expect.JQ = %q, want the body.jq folded in", td.Health.Expect.JQ)
	}
	if err := ValidateState(st); err != nil {
		t.Fatalf("ValidateState() error = %v (folded jq must compile)", err)
	}

	// Flat form still wins when both are present and non-empty.
	yamlFlat := strings.Replace(yamlBody, "jq: \"\"", "jq: '.up'", 1)
	if err := os.WriteFile(filepath.Join(dir, "tables", "webs.yaml"), []byte(yamlFlat), 0o644); err != nil {
		t.Fatal(err)
	}
	st2, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Tables[0].Health.Expect.JQ != ".up" {
		t.Fatalf("flat jq = %q, want .up (flat wins over body)", st2.Tables[0].Health.Expect.JQ)
	}
}

// TestTableMembersBothForms (v1.1 back-compat): the members list accepts the
// legacy flat shape (strings, weight = 1) and the structural shape
// ({addr, weight}); a missing weight key defaults to 1 and an explicit 0 is
// a drain. Marshalling always emits the structural form.
func TestTableMembersBothForms(t *testing.T) {
	flat := `name: webs
members:
  - 192.168.100.21
  - 192.168.100.22
`
	structured := `name: webs
members:
  - addr: 192.168.100.21
    weight: 5
  - addr: 192.168.100.22
    weight: 0
  - addr: 192.168.100.23
`
	shapes := []struct {
		name  string
		doc   string
		wantN int
	}{{"flat", flat, 2}, {"structured", structured, 3}}
	for _, sh := range shapes {
		var td TableDef
		if err := yaml.Unmarshal([]byte(sh.doc), &td); err != nil {
			t.Fatalf("%s: unmarshal: %v", sh.name, err)
		}
		if len(td.Members) != sh.wantN {
			t.Fatalf("%s: members = %d, want %d", sh.name, len(td.Members), sh.wantN)
		}
	}

	var td TableDef
	if err := yaml.Unmarshal([]byte(flat), &td); err != nil {
		t.Fatal(err)
	}
	for _, m := range td.Members {
		if m.Weight != DefaultMemberWeight {
			t.Fatalf("flat form: %s weight = %d, want %d", m.Addr, m.Weight, DefaultMemberWeight)
		}
	}

	if err := yaml.Unmarshal([]byte(structured), &td); err != nil {
		t.Fatal(err)
	}
	if td.Members[0].Weight != 5 || td.Members[1].Weight != 0 || td.Members[2].Weight != DefaultMemberWeight {
		t.Fatalf("structured weights = %v", td.Members)
	}
	if w := td.Members[1].Weight; w != 0 { // explicit 0 must survive: drain
		t.Fatalf("explicit drain weight = %d, want 0", w)
	}

	// Canonical round-trip: marshal emits the structural form only.
	out, err := yaml.Marshal(td)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "addr: 192.168.100.21") || !strings.Contains(string(out), "weight: 5") {
		t.Fatalf("canonical marshal lost the structural form:\n%s", out)
	}
	if strings.Contains(string(out), "- 192.168.100.21") {
		t.Fatalf("canonical marshal must not emit the flat form:\n%s", out)
	}
	var back TableDef
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	for i := range td.Members {
		if back.Members[i] != td.Members[i] {
			t.Fatalf("round-trip member %d = %+v, want %+v", i, back.Members[i], td.Members[i])
		}
	}

	// A weight field outside 0..100 still parses (validation rejects it).
	var bad TableDef
	if err := yaml.Unmarshal([]byte("name: webs\nmembers:\n  - addr: 1.2.3.4\n    weight: 101\n"), &bad); err != nil {
		t.Fatalf("weight 101 must parse and be rejected by validation, got %v", err)
	}
}

// writeServiceTree is writeStateTree plus a weighted table and a service.
func writeServiceTree(t *testing.T) string {
	t.Helper()
	dir := writeStateTree(t)
	svcDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(svcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svcDir, "webdist.yaml"), []byte(`service: webdist
table: webs
mode: weighted
listen:
  iface: vtnet0
  vip: 192.168.100.200
  port: 80
  proto: tcp
sticky: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tables", "webs.yaml"), []byte(`name: webs
members:
  - addr: 192.168.100.21
    weight: 5
  - addr: 192.168.100.22
    weight: 3
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestLoadStateServices: services/*.yaml are loaded in sorted order and the
// structural member weights come through.
func TestLoadStateServices(t *testing.T) {
	dir := writeServiceTree(t)
	st, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if len(st.Services) != 1 {
		t.Fatalf("services = %d, want 1", len(st.Services))
	}
	sd := st.Services[0]
	if sd.Service != "webdist" || sd.Table != "webs" || sd.EffectiveMode() != ModeWeighted || sd.Sticky {
		t.Fatalf("ServiceDef = %+v", sd)
	}
	if sd.Listen.Iface != "vtnet0" || sd.Listen.VIP != "192.168.100.200" || sd.Listen.Port != 80 || sd.Listen.Proto != "tcp" {
		t.Fatalf("Listen = %+v", sd.Listen)
	}

	// Weighted table with structural members.
	var webs *TableDef
	for i := range st.Tables {
		if st.Tables[i].Name == "webs" {
			webs = &st.Tables[i]
		}
	}
	if webs == nil || len(webs.Members) != 2 || webs.Members[0].Weight != 5 || webs.Members[1].Weight != 3 {
		t.Fatalf("webs members = %+v", webs)
	}

	// The loaded state must validate.
	if err := ValidateState(st); err != nil {
		t.Fatalf("ValidateState() error = %v", err)
	}

	// WriteService round-trips through LoadState.
	sd.Sticky = true
	if err := WriteService(dir, sd); err != nil {
		t.Fatalf("WriteService() error = %v", err)
	}
	st2, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(st2.Services) != 1 || !st2.Services[0].Sticky {
		t.Fatalf("WriteService round-trip = %+v", st2.Services)
	}
}

// TestValidateServices covers the service-level validation rules.
func TestValidateServices(t *testing.T) {
	base := func() State {
		st, err := LoadState(func() string {
			t.Helper()
			return writeServiceTree(t)
		}())
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	tests := []struct {
		name  string
		mutFn func(*State)
	}{
		{"name collides with rules anchor", func(s *State) { s.Services[0].Service = "web" }},
		{"unknown mode", func(s *State) { s.Services[0].Mode = "least-conn" }},
		{"bad iface", func(s *State) { s.Services[0].Listen.Iface = "vt net0" }},
		{"empty iface", func(s *State) { s.Services[0].Listen.Iface = "" }},
		{"bad vip", func(s *State) { s.Services[0].Listen.VIP = "192.168.100.999" }},
		{"port zero", func(s *State) { s.Services[0].Listen.Port = 0 }},
		{"port too big", func(s *State) { s.Services[0].Listen.Port = 70000 }},
		{"udp rejected in v1", func(s *State) { s.Services[0].Listen.Proto = "udp" }},
		{"missing table", func(s *State) { s.Services[0].Table = "ghost" }},
		{"weighted without active members", func(s *State) {
			for i := range s.Tables[1].Members {
				s.Tables[1].Members[i].Weight = 0
			}
		}},
		{"duplicate service", func(s *State) { s.Services = append(s.Services, s.Services[0]) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := base()
			tt.mutFn(&s)
			if err := ValidateState(s); err == nil {
				t.Fatal("ValidateState() error = nil, want error")
			}
		})
	}

	t.Run("empty mode defaults to round-robin", func(t *testing.T) {
		s := base()
		s.Services[0].Mode = ""
		if err := ValidateState(s); err != nil {
			t.Fatalf("ValidateState() error = %v", err)
		}
	})
}
