package watchers

import (
	"context"
	"log/slog"
	"time"

	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/health"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/metrics"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/peer"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/reconcile"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/render"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

// GitWatcher pulls the git state on the MASTER (default 30s), reconciles and
// counts results. The BACKUP never touches git: its config arrives via
// PeeringService/PushConfig.
type GitWatcher struct {
	Store *state.Store
	Rec   *reconcile.ConfigReconciler
	// Reg, when set, bounds what the config layer may (re-)add: only UP
	// members (nil registry = config mode, everyone healthy).
	Reg      *health.Registry
	Metrics  *metrics.Metrics
	Role     func() string
	Interval time.Duration // default 30s
	Log      *slog.Logger
}

// Run drives the loop until ctx is cancelled.
func (w GitWatcher) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = 30 * time.Second
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
			w.tick(ctx, log)
		}
	}
}

// healthyView converts the monitor registry into the config-layer filter
// (nil registry = config mode).
func (w GitWatcher) healthyView() reconcile.HealthyView {
	return registryHealthy(w.Reg)
}

// registryHealthy is the shared health filter handed to reconcile and the
// renderer: the rendering-grade view (v1.1) where a monitored table always
// has a key, so a total outage degrades the weighted chain instead of
// looking like "no health opinion". nil registry = config mode.
func registryHealthy(reg *health.Registry) reconcile.HealthyView {
	if reg == nil {
		return nil
	}
	return reg.HealthyStrict()
}

// publishPlans republishes the service probability gauge from the rendered
// plans (Reset first: drained or removed members must not linger as stale
// series) and reports degraded plans (no active member).
func publishPlans(m *metrics.Metrics, plans []render.Plan, log *slog.Logger) {
	if m == nil {
		return
	}
	m.ResetServicePlans()
	for _, p := range plans {
		for _, mp := range p.Members {
			m.SetServiceProbability(p.Service, mp.Addr, mp.Probability)
		}
		if p.Degraded && log != nil {
			log.Warn("service has no active members; anchor cleared", "service", p.Service)
		}
	}
}

func (w GitWatcher) tick(ctx context.Context, log *slog.Logger) {
	if w.Role != nil && peer.Role(w.Role()) != peer.RoleMaster {
		w.Metrics.IncGitPull("skipped")
		return
	}
	rev, err := w.Store.Pull(ctx)
	if err != nil {
		log.Warn("git pull failed", "error", err)
		w.Metrics.IncGitPull("error")
		return
	}
	st, err := state.LoadState(w.Store.Dir)
	if err != nil {
		log.Warn("load state failed", "error", err)
		w.Metrics.IncGitPull("error")
		return
	}
	// Diagnostics for the config-vs-health layering: what the config layer
	// is allowed to apply this pass (Debug is invisible at the default Info
	// level; flip with the daemon's -log level when investigating).
	healthy := w.healthyView()
	for _, td := range st.Tables {
		log.Debug("git reconcile pass", "revision", rev, "table", td.Name,
			"desired", state.Addrs(td.Members), "healthy", healthy[td.Name])
	}
	res, err := w.Rec.Reconcile(ctx, &st, rev, healthy)
	if err != nil {
		log.Error("reconcile failed", "revision", rev, "error", err)
		w.Metrics.IncGitPull("error")
		w.Metrics.IncApply(true)
		return
	}
	if res.Drift {
		log.Info("applied drift", "revision", rev, "tables", res.TablesApplied, "anchors", res.AnchorsApplied)
	}
	publishPlans(w.Metrics, res.Services, log)
	w.Metrics.IncGitPull("ok")
	w.Metrics.IncApply(false)
	w.Metrics.SetDrift(res.Drift)
}
