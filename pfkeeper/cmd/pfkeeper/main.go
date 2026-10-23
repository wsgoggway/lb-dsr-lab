// pfkeeper daemon: mTLS gRPC control plane (client 8443, peer 8444),
// reconcilers, health probe loop and Prometheus metrics (:9100), all wired
// around the git desired state and the local pf ruleset.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"flag"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	pfkeeperv1 "github.com/wsgoggway/lb-dsr-lab/pfkeeper/api/pfkeeperv1"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/health"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/metrics"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/peer"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/pfctl"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/reconcile"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/server"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/watchers"
)

// set via -ldflags at build time
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	flag.Parse()

	if *flagVersionOn {
		fmt.Printf("pfkeeper %s (%s)\n", version, commit)
		return
	}

	logger := newLogger(*flagLog)
	slog.SetDefault(logger)

	cfg, err := LoadConfig(*flagConfig)
	if err != nil {
		logger.Error("config load failed", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("pfkeeper exited with error", "error", err)
		os.Exit(1)
	}
	logger.Info("pfkeeper stopped cleanly")
}

func run(ctx context.Context, cfg *Config, logger *slog.Logger) error {
	logger.Info("pfkeeper starting", "version", version, "commit", commit, "node", cfg.Node)

	// State store.
	store := &state.Store{Dir: cfg.State.Dir, Remote: cfg.State.Remote, Branch: cfg.State.Branch, KeyPath: cfg.State.Key}
	if err := store.Ensure(ctx); err != nil {
		return fmt.Errorf("state store: %w", err)
	}

	// pfctl + reconcilers (one shared mutation mutex).
	pf := pfctl.NewClient(cfg.PfctlBin, nil)
	if ok, err := pf.Available(); !ok {
		logger.Warn("pfctl not available; reconcile calls will fail", "error", err)
	}
	mu := syncMutex()
	rec := reconcile.NewConfigReconciler(pf, mu, store, cfg.Sync.AutoHeal)
	hc, err := reconcile.NewHealthController(pf, mu, cfg.Sync.Mode)
	if err != nil {
		return fmt.Errorf("health controller: %w", err)
	}
	reg := health.NewRegistry()
	m := metrics.New(cfg.Node)

	// CARP role source + peer controller.
	carpW := &watchers.CarpWatcher{Iface: cfg.CarpIface, Metrics: m, Log: logger}
	var peerTLS *tls.Config
	if cfg.TLS.Cert != "" {
		peerTLS, err = clientTLS(cfg.TLS.Cert, cfg.TLS.Key, cfg.TLS.CA)
		if err != nil {
			return err
		}
	}
	peerCli := &peer.PeerClient{
		Endpoint: cfg.Peer.Endpoint,
		TLS:      peerTLS,
		NodeID:   cfg.Node,
		PeerID:   "peer",
	}
	ctl := peer.NewController(carpW, cfg.Node, cfg.Peer.Endpoint, peerCli, rec, hc, store)
	ctl.SetLogger(logger)
	ctl.Metrics = m

	// Bind ALL listeners before any pf mutation: a second instance (stale
	// supervisor, manual restart overlap) must die right here, at bind,
	// instead of crash-looping through the initial reconcile and re-adding
	// health-excluded members to the live tables every restart (t08 war).
	clientLis, err := net.Listen("tcp", cfg.Listen.Client)
	if err != nil {
		return fmt.Errorf("listen client %s: %w", cfg.Listen.Client, err)
	}
	peerLis, err := net.Listen("tcp", cfg.Listen.Peer)
	if err != nil {
		return fmt.Errorf("listen peer %s: %w", cfg.Listen.Peer, err)
	}
	metricsLis, err := net.Listen("tcp", cfg.Listen.Metrics)
	if err != nil {
		return fmt.Errorf("listen metrics %s: %w", cfg.Listen.Metrics, err)
	}
	logger.Info("listeners bound", "client", cfg.Listen.Client, "peer", cfg.Listen.Peer, "metrics", cfg.Listen.Metrics)

	// Initial reconcile: converge once before serving.
	if rev, err := store.Pull(ctx); err == nil {
		if st, err := state.LoadState(cfg.State.Dir); err == nil {
			if res, err := rec.Reconcile(ctx, &st, rev, nil); err != nil {
				logger.Warn("initial reconcile failed", "error", err)
			} else {
				logger.Info("initial reconcile done", "revision", rev, "tables", res.TablesApplied, "anchors", res.AnchorsApplied)
			}
		}
	} else {
		logger.Warn("initial pull failed (fresh single-node setup?)", "error", err)
	}

	// gRPC servers with mTLS + authz.
	srv := server.New(server.Config{
		Pf:        pf,
		Store:     store,
		Rec:       rec,
		Hc:        hc,
		Reg:       reg,
		Ctl:       ctl,
		PeerCli:   peerCli,
		Role:      carpW.Role,
		CarpIface: cfg.CarpIface,
		Audit:     logger,
		Version:   version,
		Commit:    commit,
		Node:      cfg.Node,
	})

	serverTLS, err := serverTLSConfig(cfg.TLS.Cert, cfg.TLS.Key, cfg.TLS.CA)
	if err != nil {
		return err
	}
	clientGRPC := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(serverTLS)),
		grpc.ChainUnaryInterceptor(server.AuthUnaryInterceptor(auditInterceptor(logger))),
	)
	pfkeeperv1.RegisterTableServiceServer(clientGRPC, srv)
	pfkeeperv1.RegisterRuleServiceServer(clientGRPC, srv)
	pfkeeperv1.RegisterHealthServiceServer(clientGRPC, srv)
	pfkeeperv1.RegisterStatusServiceServer(clientGRPC, srv)
	pfkeeperv1.RegisterAdminServiceServer(clientGRPC, srv)
	pfkeeperv1.RegisterServiceServiceServer(clientGRPC, srv)

	peerGRPC := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(serverTLS)),
		grpc.ChainUnaryInterceptor(server.AuthUnaryInterceptor(auditInterceptor(logger))),
	)
	pfkeeperv1.RegisterPeeringServiceServer(peerGRPC, srv)

	// HTTP /metrics.
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.Registry(), promhttp.HandlerOpts{}))
	metricsSrv := &http.Server{Addr: cfg.Listen.Metrics, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	// Watchers (all context-driven tickers).
	pfState := func() *state.State {
		st, err := state.LoadState(cfg.State.Dir)
		if err != nil {
			return nil
		}
		return &st
	}
	healthW := &watchers.HealthWatcher{
		Reg:     reg,
		Hc:      hc,
		Rec:     rec,
		Metrics: m,
		State:   pfState,
		PeerView: func() (map[string]string, bool) {
			return ctl.PeerView(), ctl.Snapshot().Reachable
		},
		Log: logger,
	}
	// Publish the local registry view to the peer controller after probes.
	healthW.LocalSink = ctl.SetLocalView

	gitW := watchers.GitWatcher{Store: store, Rec: rec, Metrics: m, Role: carpW.Role, Log: logger, Reg: reg}
	sysW := watchers.SysWatcher{Metrics: m, Log: logger}

	go func() { clientGRPC.Serve(clientLis) }()
	go func() { peerGRPC.Serve(peerLis) }()
	go func() { _ = metricsSrv.Serve(metricsLis) }()
	go ctl.Run(ctx)
	go gitW.Run(ctx)
	go sysW.Run(ctx)
	go carpW.Run(ctx)
	go healthW.Run(ctx)
	logger.Info("listening", "client", cfg.Listen.Client, "peer", cfg.Listen.Peer, "metrics", cfg.Listen.Metrics)

	<-ctx.Done()
	logger.Info("shutting down")
	stopped := make(chan struct{})
	go func() {
		clientGRPC.GracefulStop()
		peerGRPC.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		clientGRPC.Stop()
		peerGRPC.Stop()
	}
	_ = metricsSrv.Shutdown(context.Background())
	_ = peerCli.Close()
	return nil
}

// newLogger builds the slog JSON logger writing to stderr and optionally to
// -log file.
func newLogger(logPath string) *slog.Logger {
	writers := []io.Writer{os.Stderr}
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			writers = append(writers, f)
		}
	}
	return slog.New(slog.NewJSONHandler(io.MultiWriter(writers...), &slog.HandlerOptions{Level: slog.LevelInfo}))
}
