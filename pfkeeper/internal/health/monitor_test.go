package health

import (
	"testing"
	"time"

	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

func pass() Result { return Result{Passed: true, CheckedAt: time.Now()} }

func fail(reason string) Result { return Result{Passed: false, Reason: reason, CheckedAt: time.Now()} }

func TestMonitorRiseFallSequence(t *testing.T) {
	// rise=2, fall=3: the full transition sequence.
	m := NewMonitor("web", "192.168.100.21", state.HealthSpec{Rise: 2, Fall: 3})

	if m.State() != StateUnknown {
		t.Fatalf("initial state = %q, want INACTIVE", m.State())
	}

	type step struct {
		res       Result
		wantState string
		wantChg   bool
	}
	steps := []step{
		{pass(), StateUnknown, false},      // pass 1 of rise 2
		{pass(), StateUP, true},            // pass 2 -> UP (INACTIVE -> UP counts as change)
		{fail("connect"), StateUP, false},  // 1 of fall 3
		{fail("connect"), StateUP, false},  // 2 of fall 3
		{pass(), StateUP, false},           // streak reset by pass
		{fail("connect"), StateUP, false},  // 1 of fall 3
		{fail("timeout"), StateUP, false},  // 2 of fall 3
		{fail("timeout"), StateDOWN, true}, // 3 of fall 3 -> DOWN
		{pass(), StateDOWN, false},         // 1 of rise 2
		{pass(), StateUP, true},            // 2 of rise 2 -> UP
	}

	for i, s := range steps {
		changed, got := m.Observe(s.res)
		if got != s.wantState {
			t.Fatalf("step %d: state = %q, want %q (streak %d)", i, got, s.wantState, m.consecutive)
		}
		if changed != s.wantChg {
			t.Fatalf("step %d: changed = %v, want %v (state %q)", i, changed, s.wantChg, got)
		}
	}
}

func TestMonitorSingleRiseFall(t *testing.T) {
	m := NewMonitor("web", "192.168.100.22", state.HealthSpec{Rise: 1, Fall: 1})
	if changed, st := m.Observe(pass()); !changed || st != StateUP {
		t.Fatalf("first pass: (%v, %q), want immediate UP", changed, st)
	}
	if changed, st := m.Observe(fail("status_code")); !changed || st != StateDOWN {
		t.Fatalf("first fail: (%v, %q), want immediate DOWN", changed, st)
	}
}

func TestMonitorKeepsLastResult(t *testing.T) {
	m := NewMonitor("web", "192.168.100.23", state.HealthSpec{Rise: 1, Fall: 1})
	r := Result{Passed: false, Reason: ReasonTimeout, StatusCode: 0, CheckedAt: time.Now()}
	m.Observe(r)
	if got := m.Last(); got.Reason != ReasonTimeout {
		t.Fatalf("Last().Reason = %q, want %q", got.Reason, ReasonTimeout)
	}
}
