package watchers

import (
	"context"
	"log/slog"
	"time"

	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/metrics"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/peer"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/reconcile"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

// GitWatcher pulls the git state on the MASTER (default 30s), reconciles and
// counts results. The BACKUP never touches git: its config arrives via
// PeeringService/PushConfig.
type GitWatcher struct {
	Store    *state.Store
	Rec      *reconcile.ConfigReconciler
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
	res, err := w.Rec.Reconcile(ctx, &st, rev)
	if err != nil {
		log.Error("reconcile failed", "revision", rev, "error", err)
		w.Metrics.IncGitPull("error")
		w.Metrics.IncApply(true)
		return
	}
	if res.Drift {
		log.Info("applied drift", "revision", rev, "tables", res.TablesApplied, "anchors", res.AnchorsApplied)
	}
	w.Metrics.IncGitPull("ok")
	w.Metrics.IncApply(false)
	w.Metrics.SetDrift(res.Drift)
}
