package watchers

import (
	"context"
	"log/slog"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/metrics"
)

// SysWatcher samples host memory/CPU/load via gopsutil (default 15s) into
// the sys_* gauges.
type SysWatcher struct {
	Metrics  *metrics.Metrics
	Interval time.Duration // default 15s
	Log      *slog.Logger
}

// Run drives the loop until ctx is cancelled.
func (w SysWatcher) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = 15 * time.Second
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
			w.sample(log)
		}
	}
}

func (w SysWatcher) sample(log *slog.Logger) {
	if vm, err := mem.VirtualMemory(); err == nil {
		w.Metrics.MemUsedBytes.Set(float64(vm.Used))
		w.Metrics.MemAvailableBytes.Set(float64(vm.Available))
		w.Metrics.MemUsedPercent.Set(vm.UsedPercent)
	} else {
		log.Warn("memory sample failed", "error", err)
	}
	if avg, err := load.Avg(); err == nil {
		w.Metrics.Load1.Set(avg.Load1)
		w.Metrics.Load5.Set(avg.Load5)
		w.Metrics.Load15.Set(avg.Load15)
	}
	// Non-blocking CPU sample: percent since the previous call.
	if pct, err := cpu.Percent(0, false); err == nil && len(pct) > 0 {
		w.Metrics.CPUPercent.Set(pct[0])
	}
}
