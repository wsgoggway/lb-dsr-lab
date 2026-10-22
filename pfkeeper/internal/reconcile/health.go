package reconcile

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/health"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/pfctl"
)

// EffectiveResult reports the point changes made by one ApplyEffective pass.
type EffectiveResult struct {
	Table   string
	Added   []string
	Removed []string
}

// HealthController maintains the live membership of one table as the
// health-filtered subset of the desired members.
//
// Concurrency: mu is the pfctl mutation mutex shared with ConfigReconciler;
// the add/delete section runs under it.
type HealthController struct {
	pf        *pfctl.Client
	mu        *sync.Mutex
	consensus string // "local" | "and" | "or"
}

// NewHealthController wires the health layer. consensus must be
// local|and|or (an empty value defaults to local).
func NewHealthController(pf *pfctl.Client, mu *sync.Mutex, consensus string) (*HealthController, error) {
	switch consensus {
	case "":
		consensus = "local"
	case "local", "and", "or":
	default:
		return nil, fmt.Errorf("health: unknown consensus mode %q", consensus)
	}
	if mu == nil {
		mu = &sync.Mutex{}
	}
	return &HealthController{pf: pf, mu: mu, consensus: consensus}, nil
}

// Consensus returns the configured consensus mode.
func (h *HealthController) Consensus() string { return h.consensus }

// ApplyEffective recomputes the effective member set of table (desired
// members filtered by health consensus) and converges live pf membership to
// it with point TableAdd/TableDelete — never TableReplace, so the config
// layer's full sets are not clobbered mid-flight.
//
// Views map member address -> "UP"/"DOWN"/"INACTIVE" (from the local
// healthWatcher and the peer's PublishHealth view respectively):
//
//	local — only the local view decides;
//	and   — member is up iff local UP AND peer not-DOWN;
//	or    — member is up iff local UP OR peer UP.
//
// Two degradation rules (documented behaviour):
//
//   - when the PEER ITSELF is unreachable its view is untrusted and the
//     consensus degrades to the local view for every mode;
//   - a MISSING peerView entry is not a DOWN vote: the peer has no opinion
//     yet (its view has not arrived), so the member degrades to the local
//     view. Only an explicit peer state=DOWN counts as down.
//
// Effective is bounded by desired: members absent from desired are never
// added, and live members outside desired are left to the config layer.
func (h *HealthController) ApplyEffective(ctx context.Context, table string, desired []string, localView, peerView map[string]string, peerReachable bool) (EffectiveResult, error) {
	res := EffectiveResult{Table: table}

	effective := make(map[string]bool, len(desired))
	for _, m := range desired {
		if h.healthy(m, localView, peerView, peerReachable) {
			effective[m] = true
		}
	}

	current, err := h.pf.TableShow(ctx, table)
	if err != nil && !tableMissing(err) {
		return res, fmt.Errorf("health: table show %q: %w", table, err)
	}

	for _, m := range current {
		if !slices.Contains(desired, m) {
			continue // not ours (the config layer owns the full set)
		}
		if !effective[m] {
			res.Removed = append(res.Removed, m)
		}
	}
	for _, m := range desired {
		if effective[m] && !slices.Contains(current, m) {
			res.Added = append(res.Added, m)
		}
	}

	if len(res.Added) == 0 && len(res.Removed) == 0 {
		return res, nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range res.Added {
		if err := h.pf.TableAdd(ctx, table, m); err != nil {
			return res, fmt.Errorf("health: table add %q %q: %w", table, m, err)
		}
	}
	for _, m := range res.Removed {
		if err := h.pf.TableDelete(ctx, table, m); err != nil {
			return res, fmt.Errorf("health: table delete %q %q: %w", table, m, err)
		}
	}
	return res, nil
}

// healthy applies the consensus mode to one member. An unreachable peer
// degrades every mode to the local view; a MISSING peer entry counts as UP
// (no peer opinion yet), only an explicit DOWN votes the member down.
func (h *HealthController) healthy(member string, localView, peerView map[string]string, peerReachable bool) bool {
	localUp := localView[member] == health.StateUP
	if !peerReachable {
		return localUp
	}
	peerUp := peerView[member] != health.StateDOWN
	switch h.consensus {
	case "and":
		return localUp && peerUp
	case "or":
		return localUp || peerUp
	default: // "local"
		return localUp
	}
}

// tableMissing reports pfctl's "no such table" answer (exit 1 with the
// stderr text embedded in the wrapped error message).
func tableMissing(err error) bool {
	if err == nil {
		return false
	}
	if code, ok := pfctl.ExitCode(err); ok && code == 1 {
		return strings.Contains(strings.ToLower(err.Error()), "no table")
	}
	return false
}
