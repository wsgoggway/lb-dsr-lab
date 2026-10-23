package server

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	pfkeeperv1 "github.com/wsgoggway/lb-dsr-lab/pfkeeper/api/pfkeeperv1"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/peer"
)

// PushConfig applies an effective-config snapshot received from the MASTER
// (peer listener, node certificates only) and records it as the node's
// SyncCheck identity.
func (s *Server) PushConfig(ctx context.Context, snap *pfkeeperv1.EffectiveConfigSnapshot) (*emptypb.Empty, error) {
	if s.ctl == nil {
		return nil, status.Error(codes.FailedPrecondition, "peering is not configured on this node")
	}
	if _, err := s.ctl.ApplySnapshot(ctx, snap); err != nil {
		return nil, status.Errorf(codes.Internal, "apply snapshot: %v", err)
	}
	// The applied snapshot (not the stale local git dir) is what this BACKUP
	// reports in SyncCheck from now on.
	s.ctl.SetAppliedSnapshot(snap.GetConfigHash(), snap.GetRevision())
	return &emptypb.Empty{}, nil
}

// PublishHealth stores the peer's per-member health view (consumed by the
// MASTER's health-consensus evaluation in the healthWatcher).
func (s *Server) PublishHealth(ctx context.Context, view *pfkeeperv1.HealthView) (*emptypb.Empty, error) {
	if s.ctl == nil {
		return nil, status.Error(codes.FailedPrecondition, "peering is not configured on this node")
	}
	converted := make(map[string]string, len(view.GetMembers()))
	for _, mh := range view.GetMembers() {
		converted[peer.LocalViewKey(mh.GetTable(), mh.GetAddress())] = mh.GetState()
	}
	s.ctl.SetPeerView(converted)
	return &emptypb.Empty{}, nil
}

// SyncCheck compares the caller's config hash/revision with ours. Our own
// identity is the applied snapshot on a BACKUP and the git state on the
// MASTER (Controller.SyncIdentity). A mismatch answers diverged=true and,
// when we are the MASTER, requests an immediate heal push.
func (s *Server) SyncCheck(ctx context.Context, req *pfkeeperv1.SyncCheckRequest) (*pfkeeperv1.SyncCheckResponse, error) {
	ownHash, ownRev := s.ownIdentity(ctx)
	diverged := req.GetConfigHash() != ownHash
	if diverged && s.ctl != nil && s.role() == string(peer.RoleMaster) {
		s.ctl.RequestHeal()
	}
	return &pfkeeperv1.SyncCheckResponse{
		Diverged:       diverged,
		PeerRevision:   ownRev,
		PeerConfigHash: ownHash,
	}, nil
}

// ownIdentity is this node's config identity: the controller's reported
// identity (applied snapshot on BACKUP / store on MASTER) with a plain store
// fallback when peering is not wired (tests, single-node).
func (s *Server) ownIdentity(ctx context.Context) (hash, rev string) {
	if s.ctl != nil {
		return s.ctl.SyncIdentity(ctx)
	}
	if s.store != nil {
		if h, err := s.store.ConfigHash(ctx); err == nil {
			hash = h
		}
		if r, err := s.store.HeadRevision(ctx); err == nil {
			rev = r
		}
	}
	return hash, rev
}

// redirectMethods are the client mutations a BACKUP may proxy to the MASTER
// via RedirectWrite; everything else is refused.
var redirectMethods = map[string]func(*Server, context.Context, string, []byte) ([]byte, error){
	setTableM:        dispatchSetTable,
	addMembersM:      dispatchAddMembers,
	deleteMembersM:   dispatchDeleteMembers,
	setMemberWeightM: dispatchSetMemberWeight,
	syncM:            dispatchSync,
}

func dispatchSetTable(s *Server, ctx context.Context, actor string, payload []byte) ([]byte, error) {
	req := &pfkeeperv1.SetTableRequest{}
	if err := proto.Unmarshal(payload, req); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "bad payload: %v", err)
	}
	resp, err := s.setTableLocal(ctx, actor, req)
	if err != nil {
		return nil, err
	}
	return proto.Marshal(resp)
}

func dispatchAddMembers(s *Server, ctx context.Context, actor string, payload []byte) ([]byte, error) {
	req := &pfkeeperv1.AddMembersRequest{}
	if err := proto.Unmarshal(payload, req); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "bad payload: %v", err)
	}
	resp, err := s.addMembersLocal(ctx, actor, req)
	if err != nil {
		return nil, err
	}
	return proto.Marshal(resp)
}

func dispatchDeleteMembers(s *Server, ctx context.Context, actor string, payload []byte) ([]byte, error) {
	req := &pfkeeperv1.DeleteMembersRequest{}
	if err := proto.Unmarshal(payload, req); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "bad payload: %v", err)
	}
	resp, err := s.deleteMembersLocal(ctx, actor, req)
	if err != nil {
		return nil, err
	}
	return proto.Marshal(resp)
}

func dispatchSetMemberWeight(s *Server, ctx context.Context, actor string, payload []byte) ([]byte, error) {
	req := &pfkeeperv1.SetMemberWeightRequest{}
	if err := proto.Unmarshal(payload, req); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "bad payload: %v", err)
	}
	resp, err := s.setMemberWeightLocal(ctx, actor, req)
	if err != nil {
		return nil, err
	}
	return proto.Marshal(resp)
}

func dispatchSync(s *Server, ctx context.Context, actor string, _ []byte) ([]byte, error) {
	resp, err := s.syncLocal(ctx, actor)
	if err != nil {
		return nil, err
	}
	return proto.Marshal(resp)
}

// RedirectWrite lets a BACKUP proxy an allowed client mutation to us (the
// MASTER). The original client CN travels in actor_cn and is kept in the
// audit trail.
func (s *Server) RedirectWrite(ctx context.Context, req *pfkeeperv1.RedirectWriteRequest) (*pfkeeperv1.RedirectWriteResponse, error) {
	method := req.GetMethod()
	dispatch, ok := redirectMethods[method]
	if !ok {
		return nil, status.Errorf(codes.PermissionDenied, "method %q is not redirectable", method)
	}
	actor := req.GetActorCn()
	if actor == "" {
		actor = "unknown"
	}
	payload, err := dispatch(s, ctx, actor, req.GetPayload())
	if err != nil {
		return nil, err
	}
	return &pfkeeperv1.RedirectWriteResponse{Payload: payload}, nil
}
