package reconcile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/health"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/pfctl"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/render"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

// fakeRule answers calls whose joined argv contains Match (first match wins).
type fakeRule struct {
	match string
	out   string
	err   error
}

// fakeRunner records every argv (joined), reports the max in-flight count of
// MUTATING calls (reads may run concurrently by design) and answers calls
// via ordered rules (substring match on the argv).
type fakeRunner struct {
	rules []fakeRule
	delay time.Duration
	// Capture, when set, receives every argv (and may read files referenced
	// by it) before the rules are evaluated.
	Capture func(joined string, args []string)

	mu          sync.Mutex
	calls       []string
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
}

func (f *fakeRunner) Run(_ context.Context, _ string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	if f.Capture != nil {
		f.Capture(joined, args)
	}
	mutating := isMutationArgv(joined)
	if mutating {
		cur := f.inFlight.Add(1)
		for {
			max := f.maxInFlight.Load()
			if cur <= max || f.maxInFlight.CompareAndSwap(max, cur) {
				break
			}
		}
		defer f.inFlight.Add(-1)
	}

	f.mu.Lock()
	f.calls = append(f.calls, joined)
	rules := f.rules
	f.mu.Unlock()

	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	for _, r := range rules {
		if strings.Contains(joined, r.match) {
			return r.out, r.err
		}
	}
	return "", nil
}

// isMutationArgv reports whether an argv line mutates pf (replace/add/delete,
// anchor or main-conf load, flush, kill-states).
func isMutationArgv(joined string) bool {
	for _, m := range []string{"-T replace", "-T add", "-T delete", " -f ", "-F ", " -k "} {
		if strings.Contains(joined, m) {
			return true
		}
	}
	return false
}

func (f *fakeRunner) countCalls(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

// pfNoTableErr builds an error mimicking ExecRunner's message for
// `pfctl -t <table> -T show` on a missing table: a genuine *exec.ExitError
// (exit 1) wrapped with the "no table" stderr text.
func pfNoTableErr(t *testing.T, table string) error {
	t.Helper()
	script := fmt.Sprintf("echo 'pfctl: No table <%s> found.' >&2; exit 1", table)
	var stderr bytes.Buffer
	cmd := exec.Command("sh", "-c", script)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pfctl -t %s -T show: %w: %s", table, err, strings.TrimSpace(stderr.String()))
	}
	t.Fatal("expected non-zero exit")
	return nil
}

func testState(t *testing.T, tables ...state.TableDef) *state.State {
	t.Helper()
	st := &state.State{Anchors: map[string]string{}}
	st.Tables = append(st.Tables, tables...)
	return st
}

func TestReconcileReplaceOnlyOnDiff(t *testing.T) {
	ctx := context.Background()

	t.Run("identical members -> no replace", func(t *testing.T) {
		fr := &fakeRunner{rules: []fakeRule{
			{match: "-t web -T show", out: "10.0.0.1\n10.0.0.2\n"},
		}}
		rec := NewConfigReconciler(pfctl.NewClient("/sbin/pfctl", fr), nil, &state.Store{Dir: t.TempDir()}, true)
		st := testState(t, state.TableDef{Name: "web", Members: state.PlainMembers("10.0.0.1", "10.0.0.2")})
		res, err := rec.Reconcile(ctx, st, "abc123", nil)
		if err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if res.TablesApplied != 0 || res.Drift {
			t.Fatalf("res = %+v, want no apply no drift", res)
		}
		if fr.countCalls("-T replace") != 0 {
			t.Fatal("TableReplace called without diff")
		}
	})

	t.Run("diff -> exactly one replace with desired members", func(t *testing.T) {
		fr := &fakeRunner{rules: []fakeRule{
			{match: "-t web -T show", out: "10.0.0.1\n10.0.0.2\n10.0.0.3\n"},
		}}
		rec := NewConfigReconciler(pfctl.NewClient("/sbin/pfctl", fr), nil, &state.Store{Dir: t.TempDir()}, true)
		st := testState(t, state.TableDef{Name: "web", Members: state.PlainMembers("10.0.0.1")})
		res, err := rec.Reconcile(ctx, st, "abc123", nil)
		if err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if res.TablesApplied != 1 || !res.Drift || len(res.Diffs) != 1 {
			t.Fatalf("res = %+v, want 1 apply, drift, 1 diff", res)
		}
		if got := res.Diffs[0]; got.Table != "web" || len(got.Extra) != 2 {
			t.Fatalf("diff = %+v, want extra 10.0.0.2/10.0.0.3", got)
		}
		if fr.countCalls("-T replace") != 1 {
			t.Fatalf("replace calls = %d, want 1", fr.countCalls("-T replace"))
		}
	})

	t.Run("missing table created only with members", func(t *testing.T) {
		fr := &fakeRunner{rules: []fakeRule{
			{match: "-t db -T show", err: pfNoTableErr(t, "db")},
			{match: "-t empty -T show", err: pfNoTableErr(t, "empty")},
		}}
		rec := NewConfigReconciler(pfctl.NewClient("/sbin/pfctl", fr), nil, &state.Store{Dir: t.TempDir()}, true)

		st := testState(t,
			state.TableDef{Name: "db", Members: state.PlainMembers("10.0.0.9")},
			state.TableDef{Name: "empty", Members: nil},
		)
		res, err := rec.Reconcile(ctx, st, "abc123", nil)
		if err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if res.TablesApplied != 1 {
			t.Fatalf("TablesApplied = %d, want 1 (db only)", res.TablesApplied)
		}
		if fr.countCalls("-t db -T replace") != 1 {
			t.Fatal("db replace not called")
		}
		if fr.countCalls("-t empty -T replace") != 0 {
			t.Fatal("empty desired table must not be created")
		}
	})

	t.Run("existing table may be emptied", func(t *testing.T) {
		fr := &fakeRunner{rules: []fakeRule{
			{match: "-t web -T show", out: "10.0.0.1\n"},
		}}
		rec := NewConfigReconciler(pfctl.NewClient("/sbin/pfctl", fr), nil, &state.Store{Dir: t.TempDir()}, true)
		st := testState(t, state.TableDef{Name: "web"})
		res, err := rec.Reconcile(ctx, st, "abc123", nil)
		if err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if res.TablesApplied != 1 || fr.countCalls("-T replace") != 1 {
			t.Fatalf("res = %+v, want one emptying replace", res)
		}
	})
}

func TestReconcileAnchors(t *testing.T) {
	ctx := context.Background()

	t.Run("changed anchor is loaded into pfkeeper/<name>", func(t *testing.T) {
		fr := &fakeRunner{rules: []fakeRule{
			{match: "-a " + AnchorName("web") + " -sr", out: "pass in to <web>"},
		}}
		rec := NewConfigReconciler(pfctl.NewClient("/sbin/pfctl", fr), nil, &state.Store{Dir: t.TempDir()}, true)
		st := testState(t)
		st.Anchors["web"] = "pass in quick to <web>\n"
		res, err := rec.Reconcile(ctx, st, "abc123", nil)
		if err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if res.AnchorsApplied != 1 || !res.Drift {
			t.Fatalf("res = %+v, want 1 anchor applied with drift", res)
		}
		if fr.countCalls("-a "+AnchorName("web")+" -f ") != 1 {
			t.Fatal("LoadAnchor not called for drifted anchor")
		}
	})

	t.Run("matching anchor untouched", func(t *testing.T) {
		fr := &fakeRunner{rules: []fakeRule{
			{match: "-a " + AnchorName("web") + " -sr", out: "pass in quick to <web>"},
		}}
		rec := NewConfigReconciler(pfctl.NewClient("/sbin/pfctl", fr), nil, &state.Store{Dir: t.TempDir()}, true)
		st := testState(t)
		st.Anchors["web"] = "  pass in quick to <web>  \n"
		res, err := rec.Reconcile(ctx, st, "abc123", nil)
		if err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if res.AnchorsApplied != 0 || res.Drift {
			t.Fatalf("res = %+v, want clean", res)
		}
	})
}

func TestDetectDriftReadOnly(t *testing.T) {
	ctx := context.Background()
	fr := &fakeRunner{rules: []fakeRule{
		{match: "-t web -T show", out: "10.0.0.1\n10.0.0.99\n"},
		{match: "-a " + AnchorName("web") + " -sr", out: "stale rules"},
	}}
	rec := NewConfigReconciler(pfctl.NewClient("/sbin/pfctl", fr), nil, &state.Store{Dir: t.TempDir()}, true)
	st := testState(t, state.TableDef{Name: "web", Members: state.PlainMembers("10.0.0.1")})
	st.Anchors["web"] = "pass in quick to <web>"

	diffs, err := rec.DetectDrift(ctx, st, nil)
	if err != nil {
		t.Fatalf("DetectDrift() error = %v", err)
	}
	if len(diffs) != 1 || diffs[0].Table != "web" || len(diffs[0].Extra) != 1 {
		t.Fatalf("diffs = %+v, want web extra 10.0.0.99", diffs)
	}
	for _, mutation := range []string{"-T replace", "-T add", "-T delete", " -f "} {
		if fr.countCalls(mutation) != 0 {
			t.Fatalf("DetectDrift mutated pf with %q", mutation)
		}
	}
}

func TestApplyEffectiveConsensus(t *testing.T) {
	ctx := context.Background()
	desired := []string{"10.0.0.1", "10.0.0.2"}

	build := func(t *testing.T, consensus string, rules []fakeRule) (*fakeRunner, *HealthController) {
		t.Helper()
		fr := &fakeRunner{rules: rules}
		hc, err := NewHealthController(pfctl.NewClient("/sbin/pfctl", fr), nil, consensus)
		if err != nil {
			t.Fatal(err)
		}
		return fr, hc
	}

	t.Run("and: local UP + peer DOWN -> delete", func(t *testing.T) {
		fr, hc := build(t, "and", []fakeRule{
			{match: "-t web -T show", out: "10.0.0.1\n10.0.0.2\n"},
		})
		local := map[string]string{"10.0.0.1": health.StateUP, "10.0.0.2": health.StateUP}
		peer := map[string]string{"10.0.0.1": health.StateUP, "10.0.0.2": health.StateDOWN}
		res, err := hc.ApplyEffective(ctx, "web", desired, local, peer, true)
		if err != nil {
			t.Fatalf("ApplyEffective() error = %v", err)
		}
		if len(res.Removed) != 1 || res.Removed[0] != "10.0.0.2" || len(res.Added) != 0 {
			t.Fatalf("res = %+v, want remove 10.0.0.2", res)
		}
		if fr.countCalls("-T delete 10.0.0.2") != 1 || fr.countCalls("-T add") != 0 {
			t.Fatalf("calls: delete=%d add=%d", fr.countCalls("-T delete"), fr.countCalls("-T add"))
		}
	})

	t.Run("and: peer unreachable degrades to local (no change)", func(t *testing.T) {
		fr, hc := build(t, "and", []fakeRule{
			{match: "-t web -T show", out: "10.0.0.1\n10.0.0.2\n"},
		})
		local := map[string]string{"10.0.0.1": health.StateUP, "10.0.0.2": health.StateUP}
		peer := map[string]string{"10.0.0.2": health.StateDOWN}
		res, err := hc.ApplyEffective(ctx, "web", desired, local, peer, false)
		if err != nil {
			t.Fatalf("ApplyEffective() error = %v", err)
		}
		if len(res.Removed) != 0 || len(res.Added) != 0 || fr.countCalls("-T delete") != 0 {
			t.Fatalf("res = %+v, want no changes when peer unreachable", res)
		}
	})

	t.Run("and: local DOWN stays removed even when peer unreachable", func(t *testing.T) {
		_, hc := build(t, "and", []fakeRule{
			{match: "-t web -T show", out: "10.0.0.1\n10.0.0.2\n"},
		})
		local := map[string]string{"10.0.0.1": health.StateUP, "10.0.0.2": health.StateDOWN}
		res, err := hc.ApplyEffective(ctx, "web", desired, local, nil, false)
		if err != nil {
			t.Fatalf("ApplyEffective() error = %v", err)
		}
		if len(res.Removed) != 1 || res.Removed[0] != "10.0.0.2" {
			t.Fatalf("res = %+v, want remove 10.0.0.2", res)
		}
	})

	t.Run("or: peer UP keeps member that is locally DOWN", func(t *testing.T) {
		_, hc := build(t, "or", []fakeRule{
			{match: "-t web -T show", out: "10.0.0.1\n10.0.0.2\n"},
		})
		local := map[string]string{"10.0.0.1": health.StateUP, "10.0.0.2": health.StateDOWN}
		peer := map[string]string{"10.0.0.1": health.StateUP, "10.0.0.2": health.StateUP}
		res, err := hc.ApplyEffective(ctx, "web", desired, local, peer, true)
		if err != nil {
			t.Fatalf("ApplyEffective() error = %v", err)
		}
		if len(res.Removed) != 0 || len(res.Added) != 0 {
			t.Fatalf("res = %+v, want no changes (or keeps m2)", res)
		}
	})

	t.Run("or: peer DOWN and unreachable -> remove", func(t *testing.T) {
		_, hc := build(t, "or", []fakeRule{
			{match: "-t web -T show", out: "10.0.0.1\n10.0.0.2\n"},
		})
		local := map[string]string{"10.0.0.1": health.StateUP, "10.0.0.2": health.StateDOWN}
		peer := map[string]string{"10.0.0.2": health.StateUP}
		res, err := hc.ApplyEffective(ctx, "web", desired, local, peer, false)
		if err != nil {
			t.Fatalf("ApplyEffective() error = %v", err)
		}
		if len(res.Removed) != 1 || res.Removed[0] != "10.0.0.2" {
			t.Fatalf("res = %+v, want remove 10.0.0.2", res)
		}
	})

	t.Run("local mode ignores peer view and adds member", func(t *testing.T) {
		fr, hc := build(t, "local", []fakeRule{
			{match: "-t web -T show", out: "10.0.0.1\n"},
		})
		local := map[string]string{"10.0.0.1": health.StateUP, "10.0.0.2": health.StateUP}
		peer := map[string]string{"10.0.0.2": health.StateDOWN}
		res, err := hc.ApplyEffective(ctx, "web", desired, local, peer, true)
		if err != nil {
			t.Fatalf("ApplyEffective() error = %v", err)
		}
		if len(res.Added) != 1 || res.Added[0] != "10.0.0.2" || fr.countCalls("-T add 10.0.0.2") != 1 {
			t.Fatalf("res = %+v, want add 10.0.0.2", res)
		}
	})

	t.Run("missing table treated as empty", func(t *testing.T) {
		fr := &fakeRunner{rules: []fakeRule{
			{match: "-t web -T show", err: pfNoTableErr(t, "web")},
		}}
		hc, err := NewHealthController(pfctl.NewClient("/sbin/pfctl", fr), nil, "local")
		if err != nil {
			t.Fatal(err)
		}
		local := map[string]string{"10.0.0.1": health.StateUP}
		res, err := hc.ApplyEffective(ctx, "web", []string{"10.0.0.1"}, local, nil, false)
		if err != nil {
			t.Fatalf("ApplyEffective() error = %v", err)
		}
		if len(res.Added) != 1 || fr.countCalls("-T add 10.0.0.1") != 1 {
			t.Fatalf("res = %+v, want add into missing table", res)
		}
	})

	t.Run("invalid consensus rejected", func(t *testing.T) {
		if _, err := NewHealthController(pfctl.NewClient("/sbin/pfctl", &fakeRunner{}), nil, "quorum"); err == nil {
			t.Fatal("NewHealthController(quorum) error = nil, want error")
		}
	})
}

// TestSharedMutexSerializes runs Reconcile and ApplyEffective concurrently
// over one shared mutex and asserts pfctl calls never overlap.
func TestSharedMutexSerializes(t *testing.T) {
	fr := &fakeRunner{
		rules: []fakeRule{{match: "-t web -T show", out: "10.0.0.1\n"}},
		delay: 2 * time.Millisecond, // widen the race window
	}
	mu := &sync.Mutex{}
	pf := pfctl.NewClient("/sbin/pfctl", fr)
	rec := NewConfigReconciler(pf, mu, &state.Store{Dir: t.TempDir()}, true)
	hc, err := NewHealthController(pf, mu, "local")
	if err != nil {
		t.Fatal(err)
	}

	st := testState(t, state.TableDef{Name: "web", Members: state.PlainMembers("10.0.0.1", "10.0.0.2")})
	local := map[string]string{"10.0.0.1": health.StateUP, "10.0.0.2": health.StateUP}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = rec.Reconcile(context.Background(), st, "rev", nil)
		}()
		go func() {
			defer wg.Done()
			_, _ = hc.ApplyEffective(context.Background(), "web", state.Addrs(st.Tables[0].Members), local, nil, false)
		}()
	}
	wg.Wait()

	if got := fr.maxInFlight.Load(); got != 1 {
		t.Fatalf("max in-flight pfctl calls = %d, want 1 (mutex must serialize)", got)
	}
}

// TestApplyMainConf covers the empty-state guard and validation-before-load.
func TestApplyMainConf(t *testing.T) {
	ctx := context.Background()

	t.Run("empty state conf is an error", func(t *testing.T) {
		rec := NewConfigReconciler(pfctl.NewClient("/sbin/pfctl", &fakeRunner{}), nil, &state.Store{Dir: t.TempDir()}, true)
		if err := rec.ApplyMainConf(ctx, true); err == nil {
			t.Fatal("ApplyMainConf() error = nil on empty state, want error")
		}
	})

	t.Run("invalid ruleset rejected before load", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(dir+"/conf", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir+"/conf/pf.conf", []byte("this is not pf syntax ~~~\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		fr := &fakeRunner{rules: []fakeRule{
			{match: "-n -f", err: errors.New("pfctl: /tmp/...: syntax error")},
		}}
		rec := NewConfigReconciler(pfctl.NewClient("/sbin/pfctl", fr), nil, &state.Store{Dir: dir}, true)
		if err := rec.ApplyMainConf(ctx, false); err == nil {
			t.Fatal("ApplyMainConf() error = nil on invalid conf, want error")
		}
		// LoadMainConf argv is " -f <tmp>" without "-n": every " -f " call
		// must be the validation one ("-n -f"), none a real load.
		if fr.countCalls(" -f ")-fr.countCalls("-n -f") != 0 {
			t.Fatal("LoadMainConf must not run after failed validation")
		}
	})
}

// TestAndConsensusEmptyPeerView: a MISSING peer entry is not a DOWN vote —
// the effective set degrades to the local view (t08 regression).
func TestAndConsensusEmptyPeerView(t *testing.T) {
	ctx := context.Background()
	fr := &fakeRunner{rules: []fakeRule{
		{match: "-t web -T show", out: "10.0.0.1\n10.0.0.2\n"},
	}}
	hc, err := NewHealthController(pfctl.NewClient("/sbin/pfctl", fr), nil, "and")
	if err != nil {
		t.Fatal(err)
	}
	desired := []string{"10.0.0.1", "10.0.0.2"}
	local := map[string]string{"10.0.0.1": health.StateUP, "10.0.0.2": health.StateUP}

	// Peer reachable but its view has not arrived yet (empty map).
	res, err := hc.ApplyEffective(ctx, "web", desired, local, nil, true)
	if err != nil {
		t.Fatalf("ApplyEffective() error = %v", err)
	}
	if len(res.Added) != 0 || len(res.Removed) != 0 || fr.countCalls("-T delete") != 0 {
		t.Fatalf("res = %+v, want effective == local view (no removals)", res)
	}

	// Mixed local health with empty peer view: only the locally-DOWN member
	// is removed, the healthy one survives.
	fr2 := &fakeRunner{rules: []fakeRule{
		{match: "-t web -T show", out: "10.0.0.1\n10.0.0.2\n"},
	}}
	hc2, _ := NewHealthController(pfctl.NewClient("/sbin/pfctl", fr2), nil, "and")
	local2 := map[string]string{"10.0.0.1": health.StateUP, "10.0.0.2": health.StateDOWN}
	res2, err := hc2.ApplyEffective(ctx, "web", desired, local2, nil, true)
	if err != nil {
		t.Fatalf("ApplyEffective() error = %v", err)
	}
	if len(res2.Removed) != 1 || res2.Removed[0] != "10.0.0.2" || fr2.countCalls("-T delete 10.0.0.2") != 1 {
		t.Fatalf("res = %+v, want only the locally-DOWN member removed", res2)
	}

	// Explicit peer DOWN still removes under and-consensus.
	res3, err := hc.ApplyEffective(ctx, "web", desired, local,
		map[string]string{"10.0.0.2": health.StateDOWN}, true)
	if err != nil {
		t.Fatalf("ApplyEffective() error = %v", err)
	}
	if len(res3.Removed) != 1 || res3.Removed[0] != "10.0.0.2" {
		t.Fatalf("res = %+v, want explicit peer DOWN to remove the member", res3)
	}
}

// TestDriftRespectsHealthy (t08 layer conflict): a health-excluded member is
// never re-added by the config layer; extra members are removed; nil healthy
// keeps the old config-mode behaviour.
func TestDriftRespectsHealthy(t *testing.T) {
	desired := []string{"10.0.0.21", "10.0.0.22"}
	// Live pf has the health-excluded .21 still present plus a stray .99.
	live := "10.0.0.21\n10.0.0.22\n10.0.0.99\n"

	t.Run("health-excluded member is removed, not re-added", func(t *testing.T) {
		var replaced string
		fr := &fakeRunner{
			rules: []fakeRule{{match: "-t web -T show", out: live}},
			Capture: func(joined string, args []string) {
				if strings.Contains(joined, "-T replace") {
					b, err := os.ReadFile(args[len(args)-1])
					if err != nil {
						t.Errorf("read replace payload: %v", err)
						return
					}
					replaced = string(b)
				}
			},
		}
		rec := NewConfigReconciler(pfctl.NewClient("/sbin/pfctl", fr), nil, &state.Store{Dir: t.TempDir()}, true)
		st := testState(t, state.TableDef{Name: "web", Members: state.PlainMembers(desired...)})
		healthy := HealthyView{"web": {"10.0.0.22": true}} // .21 is DOWN

		diffs, err := rec.DetectDrift(context.Background(), st, healthy)
		if err != nil {
			t.Fatalf("DetectDrift() error = %v", err)
		}
		if len(diffs) != 1 {
			t.Fatalf("diffs = %+v, want one web diff", diffs)
		}
		for _, m := range diffs[0].Missing {
			if m == "10.0.0.21" {
				t.Fatal("health-excluded member reported as missing (would be re-added)")
			}
		}
		// extra = current - allowed: the still-present .21 plus the stray .99
		// are both slated for removal; .21 must NOT be in missing.
		if len(diffs[0].Extra) != 2 || diffs[0].Extra[0] != "10.0.0.21" || diffs[0].Extra[1] != "10.0.0.99" {
			t.Fatalf("extra = %+v, want 10.0.0.21+10.0.0.99 (current minus allowed)", diffs[0].Extra)
		}

		res, err := rec.Reconcile(context.Background(), st, "rev", healthy)
		if err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if res.TablesApplied != 1 || !res.Drift {
			t.Fatalf("res = %+v, want 1 apply with drift", res)
		}
		if replaced != "10.0.0.22\n" {
			t.Fatalf("replace payload = %q, want only the healthy member", replaced)
		}
	})

	t.Run("nil healthy keeps config mode", func(t *testing.T) {
		var replaced string
		fr := &fakeRunner{
			rules: []fakeRule{{match: "-t web -T show", out: live}},
			Capture: func(joined string, args []string) {
				if strings.Contains(joined, "-T replace") {
					b, err := os.ReadFile(args[len(args)-1])
					if err != nil {
						t.Errorf("read replace payload: %v", err)
						return
					}
					replaced = string(b)
				}
			},
		}
		rec := NewConfigReconciler(pfctl.NewClient("/sbin/pfctl", fr), nil, &state.Store{Dir: t.TempDir()}, true)
		st := testState(t, state.TableDef{Name: "web", Members: state.PlainMembers(desired...)})
		if _, err := rec.Reconcile(context.Background(), st, "rev", nil); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if replaced != "10.0.0.21\n10.0.0.22\n" {
			t.Fatalf("replace payload = %q, want the full desired set (config mode)", replaced)
		}
	})

	t.Run("table without UP members has no health opinion", func(t *testing.T) {
		// healthy has no "web" key at all -> config mode for that table.
		var replaced string
		fr := &fakeRunner{
			rules: []fakeRule{{match: "-t web -T show", out: "10.0.0.21\n"}},
			Capture: func(joined string, args []string) {
				if strings.Contains(joined, "-T replace") {
					b, err := os.ReadFile(args[len(args)-1])
					if err != nil {
						t.Errorf("read replace payload: %v", err)
						return
					}
					replaced = string(b)
				}
			},
		}
		rec := NewConfigReconciler(pfctl.NewClient("/sbin/pfctl", fr), nil, &state.Store{Dir: t.TempDir()}, true)
		st := testState(t, state.TableDef{Name: "web", Members: state.PlainMembers(desired...)})
		if _, err := rec.Reconcile(context.Background(), st, "rev", HealthyView{"other": {"1.1.1.1": true}}); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if replaced != "10.0.0.21\n10.0.0.22\n" {
			t.Fatalf("replace payload = %q, want full desired set (absent key = no opinion)", replaced)
		}
	})
}

// TestAndConsensusBothViewsAgree (t08 contract case): local and peer both
// see .21 DOWN and .22 UP — effective must be {.22} with only .21 removed.
func TestAndConsensusBothViewsAgree(t *testing.T) {
	ctx := context.Background()
	fr := &fakeRunner{rules: []fakeRule{
		{match: "-t web -T show", out: "192.168.100.21\n192.168.100.22\n"},
	}}
	hc, err := NewHealthController(pfctl.NewClient("/sbin/pfctl", fr), nil, "and")
	if err != nil {
		t.Fatal(err)
	}
	desired := []string{"192.168.100.21", "192.168.100.22"}
	local := map[string]string{
		"192.168.100.21": health.StateDOWN,
		"192.168.100.22": health.StateUP,
	}
	peer := map[string]string{
		"192.168.100.21": health.StateDOWN,
		"192.168.100.22": health.StateUP,
	}
	res, err := hc.ApplyEffective(ctx, "web", desired, local, peer, true)
	if err != nil {
		t.Fatalf("ApplyEffective() error = %v", err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != "192.168.100.21" {
		t.Fatalf("removed = %+v, want only 192.168.100.21", res.Removed)
	}
	if len(res.Added) != 0 {
		t.Fatalf("added = %+v, want none (.22 is healthy and already live)", res.Added)
	}
	if fr.countCalls("-T delete 192.168.100.21") != 1 {
		t.Fatal("delete of .21 not issued")
	}
	if fr.countCalls("-T delete 192.168.100.22") != 0 {
		t.Fatal("healthy .22 must not be deleted")
	}
}

// labService builds the PoC web service (5:3:2 over table webs).
func labService() (state.ServiceDef, state.TableDef) {
	sd := state.ServiceDef{
		Service: "web",
		Table:   "webs",
		Mode:    state.ModeWeighted,
		Listen:  state.ListenDef{Iface: "vtnet0", VIP: "192.168.100.200", Port: 80, Proto: "tcp"},
	}
	td := state.TableDef{
		Name: "webs",
		Members: []state.MemberDef{
			{Addr: "192.168.100.21", Weight: 5},
			{Addr: "192.168.100.22", Weight: 3},
			{Addr: "192.168.100.23", Weight: 2},
		},
	}
	return sd, td
}

// TestReconcileServiceAnchors: rendered service anchors flow through the
// SAME anchor mechanism as rules/*.conf (drift compare + LoadAnchor).
func TestReconcileServiceAnchors(t *testing.T) {
	ctx := context.Background()
	sd, td := labService()

	t.Run("weighted chain loaded into pfkeeper/<service>", func(t *testing.T) {
		var loaded string
		fr := &fakeRunner{Capture: func(joined string, args []string) {
			// LoadAnchor argv ends with the temp file; read it before the
			// (no-op) rules answer.
			if strings.Contains(joined, "-a "+AnchorName("web")+" -f ") {
				b, err := os.ReadFile(args[len(args)-1])
				if err != nil {
					t.Errorf("read anchor payload: %v", err)
					return
				}
				loaded = string(b)
			}
		}}
		rec := NewConfigReconciler(pfctl.NewClient("/sbin/pfctl", fr), nil, &state.Store{Dir: t.TempDir()}, true)
		st := testState(t, td)
		st.Services = []state.ServiceDef{sd}

		res, err := rec.Reconcile(ctx, st, "abc123", nil)
		if err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if res.AnchorsApplied != 1 || !res.Drift {
			t.Fatalf("res = %+v, want the service anchor applied with drift", res)
		}
		want := "pass in quick on vtnet0 route-to (vtnet0 192.168.100.21) proto tcp to 192.168.100.200 port 80 probability 50.0% keep state (sloppy)\n" +
			"pass in quick on vtnet0 route-to (vtnet0 192.168.100.22) proto tcp to 192.168.100.200 port 80 probability 60.0% keep state (sloppy)\n" +
			"pass in quick on vtnet0 route-to (vtnet0 192.168.100.23) proto tcp to 192.168.100.200 port 80 keep state (sloppy)\n"
		if loaded != want {
			t.Fatalf("loaded anchor =\n%s\nwant\n%s", loaded, want)
		}
		if len(res.Services) != 1 || len(res.Services[0].Members) != 3 {
			t.Fatalf("res.Services = %+v, want one 3-member plan", res.Services)
		}
		// The rendered anchor is merged into st.Anchors (peer snapshot picks
		// it up from there).
		if st.Anchors["web"] == "" {
			t.Fatal("rendered anchor not merged into st.Anchors")
		}
	})

	t.Run("unchanged chain is not reloaded", func(t *testing.T) {
		sd, td := labService()
		plan, err := render.Service(sd, td, nil)
		if err != nil {
			t.Fatal(err)
		}
		fr := &fakeRunner{rules: []fakeRule{
			{match: "-t webs -T show", out: "192.168.100.21\n192.168.100.22\n192.168.100.23\n"},
			{match: "-a " + AnchorName("web") + " -sr", out: strings.TrimRight(plan.Rules, "\n")},
		}}
		rec := NewConfigReconciler(pfctl.NewClient("/sbin/pfctl", fr), nil, &state.Store{Dir: t.TempDir()}, true)
		st := testState(t, td)
		st.Services = []state.ServiceDef{sd}
		res, err := rec.Reconcile(ctx, st, "abc123", nil)
		if err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if res.AnchorsApplied != 0 || res.Drift {
			t.Fatalf("res = %+v, want clean", res)
		}
	})

	t.Run("health view renormalizes the chain", func(t *testing.T) {
		var loaded string
		fr := &fakeRunner{Capture: func(joined string, args []string) {
			if strings.Contains(joined, "-a "+AnchorName("web")+" -f ") {
				b, _ := os.ReadFile(args[len(args)-1])
				loaded = string(b)
			}
		}}
		rec := NewConfigReconciler(pfctl.NewClient("/sbin/pfctl", fr), nil, &state.Store{Dir: t.TempDir()}, true)
		sd, td := labService()
		st := testState(t, td)
		st.Services = []state.ServiceDef{sd}
		// .21 DOWN: 3:2 renormalize, .21 loses its rule.
		healthy := HealthyView{"webs": {"192.168.100.21": false, "192.168.100.22": true, "192.168.100.23": true}}
		if _, err := rec.Reconcile(ctx, st, "abc123", healthy); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if strings.Contains(loaded, "192.168.100.21") {
			t.Fatalf("unhealthy member still routed:\n%s", loaded)
		}
		if !strings.Contains(loaded, "route-to (vtnet0 192.168.100.22) proto tcp to 192.168.100.200 port 80 probability 60.0%") {
			t.Fatalf("renormalized chain wrong:\n%s", loaded)
		}
	})

	t.Run("degraded service renders an empty anchor", func(t *testing.T) {
		drained := td
		for i := range drained.Members {
			drained.Members[i].Weight = 0
		}
		fr := &fakeRunner{}
		rec := NewConfigReconciler(pfctl.NewClient("/sbin/pfctl", fr), nil, &state.Store{Dir: t.TempDir()}, true)
		st := testState(t, drained)
		st.Services = []state.ServiceDef{sd}
		res, err := rec.Reconcile(ctx, st, "abc123", nil)
		if err != nil {
			t.Fatalf("Reconcile() error = %v, want nil (degraded is not fatal)", err)
		}
		if len(res.Services) != 1 || !res.Services[0].Degraded {
			t.Fatalf("res.Services = %+v, want one degraded plan", res.Services)
		}
		if st.Anchors["web"] != "" {
			t.Fatalf("degraded anchor = %q, want empty", st.Anchors["web"])
		}
	})

	t.Run("missing table fails the pass", func(t *testing.T) {
		fr := &fakeRunner{}
		rec := NewConfigReconciler(pfctl.NewClient("/sbin/pfctl", fr), nil, &state.Store{Dir: t.TempDir()}, true)
		st := testState(t)
		sd := sd
		sd.Table = "ghost"
		st.Services = []state.ServiceDef{sd}
		if _, err := rec.Reconcile(ctx, st, "abc123", nil); err == nil {
			t.Fatal("Reconcile(missing table) error = nil, want error")
		}
	})
}

// drainedTable copies td with the weights renormalized per the health view
// (used to teach the fake pf what was just loaded).
func drainedTable(td state.TableDef, healthy map[string]bool) state.TableDef {
	out := td
	out.Members = append([]state.MemberDef(nil), td.Members...)
	for i := range out.Members {
		if !healthy[out.Members[i].Addr] {
			out.Members[i].Weight = 0
		}
	}
	return out
}

// TestApplyServicesRerender: the health-transition path re-renders the
// service anchors and re-applies only the drifted ones.
func TestApplyServicesRerender(t *testing.T) {
	ctx := context.Background()
	sd, td := labService()

	t.Run("health flip reloads the anchor", func(t *testing.T) {
		plan, err := render.Service(sd, td, nil)
		if err != nil {
			t.Fatal(err)
		}
		// Live pf still carries the all-healthy chain.
		fr := &fakeRunner{rules: []fakeRule{
			{match: "-a " + AnchorName("web") + " -sr", out: strings.TrimRight(plan.Rules, "\n")},
		}}
		rec := NewConfigReconciler(pfctl.NewClient("/sbin/pfctl", fr), nil, &state.Store{Dir: t.TempDir()}, true)

		// .21 went DOWN: the rendered chain shrinks -> drift -> reload.
		healthy := HealthyView{"webs": {"192.168.100.21": false, "192.168.100.22": true, "192.168.100.23": true}}
		st := testState(t, td)
		st.Services = []state.ServiceDef{sd}
		plans, applied, err := rec.ApplyServices(ctx, st, healthy)
		if err != nil {
			t.Fatalf("ApplyServices() error = %v", err)
		}
		if applied != 1 {
			t.Fatalf("applied = %d, want 1", applied)
		}
		if len(plans[0].Members) != 2 {
			t.Fatalf("plan members = %d, want 2 after drain", len(plans[0].Members))
		}
		if fr.countCalls("-a "+AnchorName("web")+" -f ") != 1 {
			t.Fatal("drifted service anchor not reloaded")
		}

		// A second pass with the same view is a no-op (converged): teach the
		// fake pf to answer with the chain we just loaded.
		plan2, err := render.Service(sd, drainedTable(td, healthy["webs"]), healthy["webs"])
		if err != nil {
			t.Fatal(err)
		}
		fr.rules[0].out = strings.TrimRight(plan2.Rules, "\n")
		_, applied, err = rec.ApplyServices(ctx, st, healthy)
		if err != nil {
			t.Fatalf("ApplyServices() #2 error = %v", err)
		}
		if applied != 0 {
			t.Fatalf("applied #2 = %d, want 0 (converged)", applied)
		}
	})

	t.Run("rules anchors are not touched by ApplyServices", func(t *testing.T) {
		fr := &fakeRunner{rules: []fakeRule{
			// Live manual anchor differs from the state one; ApplyServices
			// must NOT reload it (that is Reconcile's job).
			{match: "-a " + AnchorName("manual") + " -sr", out: "stale"},
		}}
		rec := NewConfigReconciler(pfctl.NewClient("/sbin/pfctl", fr), nil, &state.Store{Dir: t.TempDir()}, true)
		st := testState(t, td)
		st.Services = []state.ServiceDef{sd}
		st.Anchors["manual"] = "pass in from any to <webs>"
		if _, _, err := rec.ApplyServices(ctx, st, nil); err != nil {
			t.Fatalf("ApplyServices() error = %v", err)
		}
		if fr.countCalls("-a "+AnchorName("manual")+" -f ") != 0 {
			t.Fatal("ApplyServices reloaded a manual rules anchor")
		}
	})
}
