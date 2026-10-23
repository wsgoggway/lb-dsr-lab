package render

import (
	"errors"
	"strings"
	"testing"

	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

// lab builds the PoC service: web on vtnet0/VIP:80 over table webs
// (5:3:2 weights by default).
func lab(weights ...int) (state.ServiceDef, state.TableDef) {
	addrs := []string{"192.168.100.21", "192.168.100.22", "192.168.100.23"}
	members := make([]state.MemberDef, 0, len(addrs))
	for i, a := range addrs {
		w := state.DefaultMemberWeight
		if i < len(weights) {
			w = weights[i]
		}
		members = append(members, state.MemberDef{Addr: a, Weight: w})
	}
	sd := state.ServiceDef{
		Service: "web",
		Table:   "webs",
		Mode:    state.ModeWeighted,
		Listen:  state.ListenDef{Iface: "vtnet0", VIP: "192.168.100.200", Port: 80, Proto: "tcp"},
	}
	td := state.TableDef{Name: "webs", Members: members}
	return sd, td
}

func TestServiceWeightedGolden(t *testing.T) {
	sd, td := lab(5, 3, 2)
	plan, err := Service(sd, td, nil)
	if err != nil {
		t.Fatalf("Service() error = %v", err)
	}
	want := strings.Join([]string{
		"pass in quick on vtnet0 route-to (vtnet0 192.168.100.21) proto tcp to 192.168.100.200 port 80 probability 50.0% keep state (sloppy)",
		"pass in quick on vtnet0 route-to (vtnet0 192.168.100.22) proto tcp to 192.168.100.200 port 80 probability 60.0% keep state (sloppy)",
		"pass in quick on vtnet0 route-to (vtnet0 192.168.100.23) proto tcp to 192.168.100.200 port 80 keep state (sloppy)",
		"",
	}, "\n")
	if plan.Rules != want {
		t.Fatalf("rules =\n%s\nwant\n%s", plan.Rules, want)
	}
	if len(plan.Members) != 3 {
		t.Fatalf("members = %d, want 3", len(plan.Members))
	}
	// Published probabilities are shares of the TOTAL flow: 50/30/20.
	wantShares := []float64{50, 30, 20}
	for i, mp := range plan.Members {
		if mp.Probability != wantShares[i] {
			t.Errorf("member %d share = %v, want %v", i, mp.Probability, wantShares[i])
		}
		if mp.Addr != "192.168.100.2"+string(rune('1'+i)) {
			t.Errorf("member %d addr = %s", i, mp.Addr)
		}
	}
}

func TestServiceWeightedTieBreakByAddress(t *testing.T) {
	sd, td := lab(2, 2, 1)
	plan, err := Service(sd, td, nil)
	if err != nil {
		t.Fatalf("Service() error = %v", err)
	}
	lines := strings.Split(strings.TrimRight(plan.Rules, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	// Equal weights -> ascending address order; the last rule stays catch-all.
	if !strings.Contains(lines[0], "route-to (vtnet0 192.168.100.21)") ||
		!strings.Contains(lines[1], "route-to (vtnet0 192.168.100.22)") ||
		!strings.Contains(lines[2], "route-to (vtnet0 192.168.100.23)") {
		t.Fatalf("tie-break order wrong:\n%s", plan.Rules)
	}
	// 2:2:1 chain: 40% of the full flow, then 66.7% of the remainder.
	if !strings.HasSuffix(lines[0], "probability 40.0% keep state (sloppy)") {
		t.Fatalf("first rule = %s, want probability 40.0%%", lines[0])
	}
	if !strings.HasSuffix(lines[1], "probability 66.7% keep state (sloppy)") {
		t.Fatalf("second rule = %s, want probability 66.7%%", lines[1])
	}
	if strings.Contains(lines[2], "probability") {
		t.Fatalf("last rule = %s, want unconditional catch-all", lines[2])
	}
}

func TestServiceWeightedDrainRerenormalizes(t *testing.T) {
	// .21 drained (weight 0): 3:2 renormalize to 60%/catch-all.
	sd, td := lab(0, 3, 2)
	plan, err := Service(sd, td, nil)
	if err != nil {
		t.Fatalf("Service() error = %v", err)
	}
	lines := strings.Split(strings.TrimRight(plan.Rules, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2 (drained member gets no rule)", len(lines))
	}
	if !strings.Contains(lines[0], "route-to (vtnet0 192.168.100.22)") ||
		!strings.HasSuffix(lines[0], "probability 60.0% keep state (sloppy)") {
		t.Fatalf("first rule = %s, want .22 at 60.0%%", lines[0])
	}
	if !strings.Contains(lines[1], "route-to (vtnet0 192.168.100.23)") ||
		strings.Contains(lines[1], "probability") {
		t.Fatalf("last rule = %s, want unconditional catch-all", lines[1])
	}
}

func TestServiceWeightedUnhealthyExcluded(t *testing.T) {
	sd, td := lab(5, 3, 2)
	// .21 is DOWN: chain renormalizes over 3:2 — the PoC drainage case.
	view := HealthyView{"webs": {"192.168.100.21": false, "192.168.100.22": true, "192.168.100.23": true}}
	plan, err := Service(sd, td, view["webs"])
	if err != nil {
		t.Fatalf("Service() error = %v", err)
	}
	if strings.Contains(plan.Rules, "192.168.100.21") {
		t.Fatalf("unhealthy member still in the chain:\n%s", plan.Rules)
	}
	lines := strings.Split(strings.TrimRight(plan.Rules, "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "probability 60.0%") {
		t.Fatalf("rules =\n%s\nwant 3:2 renormalized chain", plan.Rules)
	}
}

func TestServiceWeightedSingleActiveMember(t *testing.T) {
	sd, td := lab(7, 0, 0)
	plan, err := Service(sd, td, nil)
	if err != nil {
		t.Fatalf("Service() error = %v", err)
	}
	want := "pass in quick on vtnet0 route-to (vtnet0 192.168.100.21) proto tcp to 192.168.100.200 port 80 keep state (sloppy)\n"
	if plan.Rules != want {
		t.Fatalf("rules = %q, want the single unconditional rule", plan.Rules)
	}
	if len(plan.Members) != 1 || plan.Members[0].Probability != 100 {
		t.Fatalf("members = %+v, want one member at 100%%", plan.Members)
	}
}

func TestServiceWeightedNoActiveMembers(t *testing.T) {
	sd, td := lab(0, 0, 0)
	_, err := Service(sd, td, nil)
	if !errors.Is(err, ErrNoActiveMembers) {
		t.Fatalf("Service() error = %v, want ErrNoActiveMembers", err)
	}

	// Same via the health view: everyone DOWN.
	sd2, td2 := lab(5, 3, 2)
	down := map[string]bool{"192.168.100.21": false, "192.168.100.22": false, "192.168.100.23": false}
	if _, err := Service(sd2, td2, down); !errors.Is(err, ErrNoActiveMembers) {
		t.Fatalf("Service(all down) error = %v, want ErrNoActiveMembers", err)
	}

	// All() merges the empty anchor and flags the plan Degraded.
	st := &state.State{
		Tables:   []state.TableDef{td},
		Services: []state.ServiceDef{sd},
		Anchors:  map[string]string{},
	}
	plans, err := All(st, nil)
	if err != nil {
		t.Fatalf("All() error = %v, want nil (degraded is not fatal)", err)
	}
	if len(plans) != 1 || !plans[0].Degraded || plans[0].Rules != "" {
		t.Fatalf("plans = %+v, want one degraded plan with an empty anchor", plans)
	}
	if got := st.Anchors["web"]; got != "" {
		t.Fatalf("anchor = %q, want cleared", got)
	}
}

func TestServiceWeightedSticky(t *testing.T) {
	sd, td := lab(1, 1)
	sd.Sticky = true
	plan, err := Service(sd, td, nil)
	if err != nil {
		t.Fatalf("Service() error = %v", err)
	}
	if !strings.Contains(plan.Rules, "route-to (vtnet0 192.168.100.21) sticky-address proto tcp") {
		t.Fatalf("sticky-address must follow the route-to pool:\n%s", plan.Rules)
	}
}

func TestServiceWeightedFractionRounding(t *testing.T) {
	// 1:1:1:1 -> 25% / 33.3% / 50% / catch-all: the second probability is a
	// repeating fraction and must render with exactly one decimal.
	addrs := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"}
	td := state.TableDef{Name: "web", Members: state.PlainMembers(addrs...)}
	sd := state.ServiceDef{
		Service: "web", Table: "web", Mode: state.ModeWeighted,
		Listen: state.ListenDef{Iface: "em0", VIP: "10.0.0.200", Port: 8080, Proto: "tcp"},
	}
	plan, err := Service(sd, td, nil)
	if err != nil {
		t.Fatalf("Service() error = %v", err)
	}
	lines := strings.Split(strings.TrimRight(plan.Rules, "\n"), "\n")
	wantProbs := []string{"probability 25.0%", "probability 33.3%", "probability 50.0%", ""}
	for i, want := range wantProbs {
		if want == "" {
			if strings.Contains(lines[i], "probability") {
				t.Errorf("line %d = %s, want catch-all", i, lines[i])
			}
			continue
		}
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d = %s, want %s", i, lines[i], want)
		}
	}
}

func TestServiceRoundRobinGolden(t *testing.T) {
	sd, td := lab(5, 3)
	sd.Mode = state.ModeRoundRobin
	sd.Sticky = false // legacy RR keeps sticky-address regardless
	plan, err := Service(sd, td, nil)
	if err != nil {
		t.Fatalf("Service() error = %v", err)
	}
	want := "pass in quick on vtnet0 route-to (vtnet0 <webs>) round-robin sticky-address proto tcp to 192.168.100.200 port 80 keep state (sloppy)\n"
	if plan.Rules != want {
		t.Fatalf("rules = %q, want the legacy round-robin rule", plan.Rules)
	}
}

func TestServiceUnknownModeRejected(t *testing.T) {
	sd, td := lab(1)
	sd.Mode = "least-conn"
	if _, err := Service(sd, td, nil); err == nil {
		t.Fatal("Service(unknown mode) error = nil, want error")
	}
}

func TestAllMergesAnchorsAndOrdersDeterministically(t *testing.T) {
	sd, td := lab(5, 3, 2)
	other := state.ServiceDef{
		Service: "api", Table: "webs", Mode: state.ModeWeighted,
		Listen: state.ListenDef{Iface: "vtnet0", VIP: "192.168.100.201", Port: 8080, Proto: "tcp"},
	}
	st := &state.State{
		Tables:   []state.TableDef{td},
		Services: []state.ServiceDef{sd, other},
		Anchors:  map[string]string{"manual": "pass in from any"},
	}
	plans, err := All(st, nil)
	if err != nil {
		t.Fatalf("All() error = %v", err)
	}
	if len(plans) != 2 || plans[0].Service != "web" || plans[1].Service != "api" {
		t.Fatalf("plans order = %v/%v, want stored order", plans[0].Service, plans[1].Service)
	}
	if st.Anchors["web"] == "" || st.Anchors["api"] == "" {
		t.Fatalf("anchors not merged: %v", st.Anchors)
	}
	if st.Anchors["manual"] != "pass in from any" {
		t.Fatal("manual anchor clobbered by render.All")
	}
	if plans[1].Anchor != "api" {
		t.Fatalf("anchor name = %q, want the service name", plans[1].Anchor)
	}
}

func TestAllMissingTableIsError(t *testing.T) {
	sd, _ := lab(1)
	sd.Table = "ghost"
	st := &state.State{Services: []state.ServiceDef{sd}, Anchors: map[string]string{}}
	if _, err := All(st, nil); err == nil {
		t.Fatal("All(missing table) error = nil, want error")
	}
}

// TestChainSumsToOne sanity-checks the published shares across a batch of
// weight combinations: they must sum to ~100 percent.
func TestChainSumsToOne(t *testing.T) {
	cases := [][]int{
		{5, 3, 2}, {1, 1}, {7, 0, 3}, {100, 1}, {33, 33, 34}, {1, 2, 3, 4, 5},
	}
	for _, weights := range cases {
		sd, td := lab(weights...)
		plan, err := Service(sd, td, nil)
		if err != nil {
			t.Fatalf("Service(%v) error = %v", weights, err)
		}
		sum := 0.0
		for _, mp := range plan.Members {
			sum += mp.Probability
		}
		if sum < 99.7 || sum > 100.3 {
			t.Errorf("weights %v: shares sum = %v, want ~100 (one-decimal rounding)", weights, sum)
		}
	}
}
