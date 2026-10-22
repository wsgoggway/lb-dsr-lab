package server

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pfkeeperv1 "github.com/wsgoggway/lb-dsr-lab/pfkeeper/api/pfkeeperv1"
)

// ListHealth returns the per-member health view of this node.
func (s *Server) ListHealth(ctx context.Context, req *pfkeeperv1.ListHealthRequest) (*pfkeeperv1.ListHealthResponse, error) {
	members := make([]*pfkeeperv1.MemberHealth, 0)
	for _, m := range s.reg.List() {
		if req.GetTable() != "" && m.Table != req.GetTable() {
			continue
		}
		members = append(members, memberToProto(m))
	}
	return &pfkeeperv1.ListHealthResponse{Members: members}, nil
}

// Inspect returns the full last probe result for one member.
func (s *Server) Inspect(ctx context.Context, req *pfkeeperv1.InspectRequest) (*pfkeeperv1.InspectResponse, error) {
	m, ok := s.reg.Get(req.GetTable(), req.GetAddress())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no monitor for %s/%s", req.GetTable(), req.GetAddress())
	}
	return &pfkeeperv1.InspectResponse{Member: memberToProto(m)}, nil
}

// ForceCheck runs an immediate probe bypassing the interval. The probe is
// always local: it observes THIS node's view of the member (the peer keeps
// its own via its own ForceCheck).
func (s *Server) ForceCheck(ctx context.Context, req *pfkeeperv1.ForceCheckRequest) (*pfkeeperv1.MemberHealth, error) {
	if _, err := s.reg.ForceCheck(ctx, req.GetTable(), req.GetAddress()); err != nil {
		return nil, status.Errorf(codes.Internal, "force check: %v", err)
	}
	m, ok := s.reg.Get(req.GetTable(), req.GetAddress())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no monitor for %s/%s", req.GetTable(), req.GetAddress())
	}
	return memberToProto(m), nil
}
