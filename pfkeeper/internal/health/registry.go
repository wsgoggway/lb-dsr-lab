package health

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

// Registry is the in-process map of per-(table, member) monitors. The
// healthWatcher owns its lifecycle (registers/removes as the desired state
// changes); the gRPC server reads it for ListHealth/Inspect/ForceCheck.
type Registry struct {
	mu       sync.RWMutex
	monitors map[monitorKey]*Monitor
}

type monitorKey struct {
	table string
	addr  string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{monitors: map[monitorKey]*Monitor{}}
}

// Register creates (or refreshes the spec of) the monitor for (table, addr)
// and returns it. An existing monitor keeps its streak.
func (r *Registry) Register(table, addr string, spec state.HealthSpec) *Monitor {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := monitorKey{table, addr}
	if m, ok := r.monitors[key]; ok {
		m.Spec = spec
		return m
	}
	m := NewMonitor(table, addr, spec)
	r.monitors[key] = m
	return m
}

// Remove drops the monitor for (table, addr); reports whether it existed.
func (r *Registry) Remove(table, addr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := monitorKey{table, addr}
	if _, ok := r.monitors[key]; !ok {
		return false
	}
	delete(r.monitors, key)
	return true
}

// Get returns the monitor for (table, addr).
func (r *Registry) Get(table, addr string) (*Monitor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.monitors[monitorKey{table, addr}]
	return m, ok
}

// List returns a snapshot of all monitors ordered by (table, address).
func (r *Registry) List() []*Monitor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Monitor, 0, len(r.monitors))
	for _, m := range r.monitors {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		return out[i].Address < out[j].Address
	})
	return out
}

// ForceCheck probes (table, addr) immediately, folds the result into the
// monitor's state machine and returns it.
func (r *Registry) ForceCheck(ctx context.Context, table, addr string) (Result, error) {
	m, ok := r.Get(table, addr)
	if !ok {
		return Result{}, fmt.Errorf("health: no monitor for %s/%s", table, addr)
	}
	prober := Pick(m.Spec)
	if prober == nil {
		return Result{}, fmt.Errorf("health: unknown prober type %q", m.Spec.Type)
	}
	res := prober.Probe(ctx, addr, m.Spec)
	m.Observe(res)
	return res, nil
}

// View returns the whole registry as "table/addr" -> UP|DOWN|INACTIVE (the
// format published to the peer and fed into the health consensus).
func (r *Registry) View() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.monitors))
	for _, m := range r.monitors {
		out[m.Table+"/"+m.Address] = m.State()
	}
	return out
}

// Healthy returns the config-layer health filter: table -> set of UP
// addresses. Tables with no UP members get no key at all — an absent key
// (or a nil map) means "no health opinion yet, treat everyone as healthy",
// so the config layer never fights the health layer on startup (only an
// explicit DOWN/INACTIVE-vs-UP decision inside a present key excludes a
// member).
func (r *Registry) Healthy() map[string]map[string]bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string]map[string]bool{}
	for _, m := range r.monitors {
		if m.State() != StateUP {
			continue
		}
		if out[m.Table] == nil {
			out[m.Table] = map[string]bool{}
		}
		out[m.Table][m.Address] = true
	}
	if len(out) == 0 {
		return nil // no UP members anywhere: no opinion, config mode
	}
	return out
}
