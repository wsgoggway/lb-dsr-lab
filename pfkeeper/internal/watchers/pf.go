// Package watchers owns the daemon's periodic loops. Every watcher runs a
// time.Ticker (never time.Sleep), honours context cancellation and reports
// into the metrics registry.
package watchers

import (
	"context"
	"log/slog"
	"time"

	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/metrics"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/pfctl"
)

// PfWatcher polls `pfctl -si` (default 10s) and publishes pf gauges.
type PfWatcher struct {
	Pf       *pfctl.Client
	Metrics  *metrics.Metrics
	Interval time.Duration // default 10s
	Log      *slog.Logger
}

// Run drives the loop until ctx is cancelled.
func (w PfWatcher) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = 10 * time.Second
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
			info, err := w.Pf.Stats(ctx)
			if err != nil {
				log.Warn("pf stats poll failed", "error", err)
				continue
			}
			w.Metrics.SetPf(info.StatesEntries, info.StatesSearches, info.StatesInserts, info.StatesRemovals, info.Match, info.StateMismatch)
		}
	}
}
