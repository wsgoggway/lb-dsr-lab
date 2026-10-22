package watchers

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/metrics"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/pfctl"
)

// CarpWatcher polls the CARP role (default 5s), publishes the carp_role
// gauge and caches the role: it is the peer.RoleSource for the peering
// controller (the M6/M7 glue).
type CarpWatcher struct {
	Iface    string
	Metrics  *metrics.Metrics
	Interval time.Duration // default 5s
	Log      *slog.Logger

	mu   sync.RWMutex
	role string
}

// Run drives the loop until ctx is cancelled.
func (w *CarpWatcher) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	log := w.Log
	if log == nil {
		log = slog.Default()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st, err := pfctl.CarpRole(ctx, w.Iface)
			if err != nil {
				log.Warn("carp poll failed", "interface", w.Iface, "error", err)
				continue
			}
			w.setRole(st.Role)
			w.Metrics.SetCarpRole(st.Role)
		}
	}
}

func (w *CarpWatcher) setRole(role string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.role = role
}

// Role implements peer.RoleSource.
func (w *CarpWatcher) Role() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.role
}
