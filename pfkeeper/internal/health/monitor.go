package health

import (
	"fmt"

	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

// Monitor is the per-(table, member) rise/fall state machine. It is not
// safe for concurrent use: the owning scheduler probes a member serially.
type Monitor struct {
	Table   string
	Address string
	Spec    state.HealthSpec

	consecutive int    // >0: pass streak; <0: failure streak; 0: fresh
	state       string // StateUP | StateDOWN | StateUnknown
	last        Result
}

// NewMonitor creates a monitor in the INACTIVE state: the first probes build
// the initial streak, and the state settles on UP only after `rise` passes
// (resp. DOWN after `fall` failures).
func NewMonitor(table, address string, spec state.HealthSpec) *Monitor {
	return &Monitor{
		Table:   table,
		Address: address,
		Spec:    spec,
		state:   StateUnknown,
	}
}

// State returns the current member state.
func (m *Monitor) State() string { return m.state }

// Last returns the most recent raw probe result.
func (m *Monitor) Last() Result { return m.last }

func (m *Monitor) rise() int {
	if m.Spec.Rise < 1 {
		return 1
	}
	return m.Spec.Rise
}

func (m *Monitor) fall() int {
	if m.Spec.Fall < 1 {
		return 1
	}
	return m.Spec.Fall
}

// Observe folds a probe result into the streak counters and returns
// (changed=true, newState) when the member state transitioned. The first
// streaks also count as changes (INACTIVE -> UP/DOWN).
func (m *Monitor) Observe(r Result) (changed bool, newState string) {
	m.last = r
	if r.Passed {
		if m.consecutive < 0 {
			m.consecutive = 0
		}
		m.consecutive++
		if m.state != StateUP && m.consecutive >= m.rise() {
			m.state = StateUP
			changed = true
		}
	} else {
		if m.consecutive > 0 {
			m.consecutive = 0
		}
		m.consecutive--
		if m.state != StateDOWN && m.consecutive <= -m.fall() {
			m.state = StateDOWN
			changed = true
		}
	}
	return changed, m.state
}

// String identifies the monitor in logs: "<table>/<address>".
func (m *Monitor) String() string {
	return fmt.Sprintf("%s/%s", m.Table, m.Address)
}
