package server

import (
	"context"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pfkeeperv1 "github.com/wsgoggway/lb-dsr-lab/pfkeeper/api/pfkeeperv1"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/health"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/peer"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/pfctl"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/reconcile"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

// Server implements all six pfkeeperv1 services. One instance serves both
// listeners: the client listener (8443) exposes Table/Rule/Health/Status/
// Admin, the peer listener (8444) exposes PeeringService; the auth
// interceptor restricts each certificate role to its subset.
type Server struct {
	pf    *pfctl.Client
	store *state.Store
	rec   *reconcile.ConfigReconciler
	hc    *reconcile.HealthController
	reg   *health.Registry
	ctl   *peer.Controller // may be nil (single-node tests)
	// peerCli proxies client mutations to the MASTER when this node is BACKUP.
	peerCli   *peer.PeerClient
	role      func() string
	carpIface string // interface polled for the carp: role line
	auditL    *slog.Logger

	version string
	commit  string
	node    string

	pfkeeperv1.UnimplementedTableServiceServer
	pfkeeperv1.UnimplementedRuleServiceServer
	pfkeeperv1.UnimplementedHealthServiceServer
	pfkeeperv1.UnimplementedStatusServiceServer
	pfkeeperv1.UnimplementedAdminServiceServer
	pfkeeperv1.UnimplementedServiceServiceServer
	pfkeeperv1.UnimplementedPeeringServiceServer
}

// Config wires the server dependencies; zero-value optional fields (Ctl,
// PeerCli, Role, CarpIface) degrade gracefully for single-node setups and
// tests.
type Config struct {
	Pf        *pfctl.Client
	Store     *state.Store
	Rec       *reconcile.ConfigReconciler
	Hc        *reconcile.HealthController
	Reg       *health.Registry
	Ctl       *peer.Controller
	PeerCli   *peer.PeerClient
	Role      func() string
	CarpIface string
	Audit     *slog.Logger
	Version   string
	Commit    string
	Node      string
}

// New builds the server.
func New(cfg Config) *Server {
	if cfg.Audit == nil {
		cfg.Audit = slog.Default()
	}
	if cfg.Role == nil {
		cfg.Role = func() string { return "INIT" }
	}
	return &Server{
		pf:        cfg.Pf,
		store:     cfg.Store,
		rec:       cfg.Rec,
		hc:        cfg.Hc,
		reg:       cfg.Reg,
		ctl:       cfg.Ctl,
		peerCli:   cfg.PeerCli,
		role:      cfg.Role,
		carpIface: cfg.CarpIface,
		auditL:    cfg.Audit,
		version:   cfg.Version,
		commit:    cfg.Commit,
		node:      cfg.Node,
	}
}

// auditSuccess/AuditErr wrappers keep call sites short.
func (s *Server) audit(actorCN, method string, params map[string]string, outcome string) {
	Audit(s.auditL, actorCN, method, params, outcome)
}

// healthyMap converts the monitor registry into the health filter for
// reconcile and rendering (table -> addresses not known DOWN, see
// Registry.HealthyStrict); nil registry = config mode (everyone healthy).
func (s *Server) healthyMap() reconcile.HealthyView {
	if s.reg == nil {
		return nil
	}
	return s.reg.HealthyStrict()
}

// loadState reads the current desired state from the store working tree.
func (s *Server) loadState() (*state.State, string, error) {
	st, err := state.LoadState(s.store.Dir)
	if err != nil {
		return nil, "", status.Errorf(codes.Internal, "load state: %v", err)
	}
	rev, err := s.store.HeadRevision(context.Background())
	if err != nil {
		rev = ""
	}
	return &st, rev, nil
}

// shouldRedirect reports whether client mutations must be proxied to the
// MASTER (this node is BACKUP and the peer is reachable).
func (s *Server) shouldRedirect() bool {
	return s.peerCli != nil && s.role() == string(peer.RoleBackup) &&
		s.ctl != nil && s.ctl.Snapshot().Reachable
}

// redirect proxies a marshalled mutation to the MASTER and fills resp with
// the unmarshalled response payload.
func (s *Server) redirect(ctx context.Context, method string, req, resp proto.Message) error {
	payload, err := proto.Marshal(req)
	if err != nil {
		return status.Errorf(codes.Internal, "marshal %s: %v", method, err)
	}
	out, err := s.peerCli.ProxyToMaster(ctx, method, payload, actorCN(ctx))
	if err != nil {
		return status.Errorf(codes.Unavailable, "proxy to master: %v", err)
	}
	if err := proto.Unmarshal(out, resp); err != nil {
		return status.Errorf(codes.Internal, "unmarshal %s response: %v", method, err)
	}
	return nil
}

// actorCN returns the caller CN attached by the auth interceptor.
func actorCN(ctx context.Context) string {
	if id, ok := IdentityFromContext(ctx); ok {
		return id.CN
	}
	return "unknown"
}
