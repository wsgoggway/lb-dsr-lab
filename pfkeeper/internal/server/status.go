package server

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	pfkeeperv1 "github.com/wsgoggway/lb-dsr-lab/pfkeeper/api/pfkeeperv1"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/peer"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/pfctl"
)

// GetPfStats returns pf runtime statistics (`pfctl -si`).
func (s *Server) GetPfStats(ctx context.Context, _ *emptypb.Empty) (*pfkeeperv1.PfStats, error) {
	info, err := s.pf.Stats(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "pf stats: %v", err)
	}
	return &pfkeeperv1.PfStats{
		StatesEntries:        info.StatesEntries,
		StatesSearches:       info.StatesSearches,
		StatesInserts:        info.StatesInserts,
		StatesRemovals:       info.StatesRemovals,
		CounterMatch:         info.Match,
		CounterStateMismatch: info.StateMismatch,
		CollectedAt:          timestamppb.Now(),
	}, nil
}

// CarpState returns the CARP role of the monitored interface.
func (s *Server) CarpState(ctx context.Context, _ *emptypb.Empty) (*pfkeeperv1.CarpStateResponse, error) {
	if s.carpIface == "" {
		return nil, status.Error(codes.FailedPrecondition, "carp_interface is not configured")
	}
	st, err := pfctl.CarpRole(ctx, s.carpIface)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "carp state: %v", err)
	}
	return &pfkeeperv1.CarpStateResponse{Role: st.Role, Vhid: st.Vhid, Interface: st.Interface}, nil
}

// PeerState reports what this node knows about its peer.
func (s *Server) PeerState(ctx context.Context, _ *emptypb.Empty) (*pfkeeperv1.PeerStateResponse, error) {
	if s.ctl == nil {
		return &pfkeeperv1.PeerStateResponse{Reachable: false}, nil
	}
	st := s.ctl.Snapshot()
	return &pfkeeperv1.PeerStateResponse{
		Reachable:    st.Reachable,
		PeerRole:     peerRoleGuess(s.role()),
		PeerRevision: st.PeerRevision,
		LagSeconds:   st.LagSeconds,
		Diverged:     st.Diverged,
	}, nil
}

// peerRoleGuess infers the peer's role from ours (a CARP pair is exclusive).
func peerRoleGuess(selfRole string) string {
	if selfRole == string(peer.RoleMaster) {
		return string(peer.RoleBackup)
	}
	if selfRole == string(peer.RoleBackup) {
		return string(peer.RoleMaster)
	}
	return "UNKNOWN"
}

// Version returns build metadata.
func (s *Server) Version(ctx context.Context, _ *emptypb.Empty) (*pfkeeperv1.VersionResponse, error) {
	return &pfkeeperv1.VersionResponse{Version: s.version, Commit: s.commit, Node: s.node}, nil
}
