package server

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pfkeeperv1 "github.com/wsgoggway/lb-dsr-lab/pfkeeper/api/pfkeeperv1"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

const (
	adminSvc  = "/pfkeeper.v1.AdminService/"
	syncM     = adminSvc + "Sync"
	rollbackM = adminSvc + "Rollback"
)

// Sync pulls the git state and reconciles now. On a BACKUP with a reachable
// peer the whole operation is proxied to the MASTER (the BACKUP never
// touches git itself).
func (s *Server) Sync(ctx context.Context, _ *emptypb.Empty) (*pfkeeperv1.SyncResponse, error) {
	if s.shouldRedirect() {
		resp := &pfkeeperv1.SyncResponse{}
		if err := s.redirect(ctx, syncM, &emptypb.Empty{}, resp); err != nil {
			s.audit(actorCN(ctx), syncM, nil, "redirect failed")
			return nil, err
		}
		s.audit(actorCN(ctx), syncM, nil, "redirected to master")
		return resp, nil
	}
	return s.syncLocal(ctx, actorCN(ctx))
}

func (s *Server) syncLocal(ctx context.Context, actor string) (*pfkeeperv1.SyncResponse, error) {
	rev, err := s.store.Pull(ctx)
	if err != nil {
		s.audit(actor, syncM, nil, "pull failed: "+err.Error())
		return nil, status.Errorf(codes.Internal, "git pull: %v", err)
	}
	st, err := state.LoadState(s.store.Dir)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load state: %v", err)
	}
	res, err := s.rec.Reconcile(ctx, &st, rev, s.healthyMap())
	if err != nil {
		s.audit(actor, syncM, nil, "reconcile failed: "+err.Error())
		return nil, status.Errorf(codes.Internal, "reconcile: %v", err)
	}
	s.audit(actor, syncM, nil, "ok revision="+rev)
	return &pfkeeperv1.SyncResponse{Revision: rev, AppliedTables: int32(res.TablesApplied)}, nil
}

// Rollback checks out the given commit and reconciles.
func (s *Server) Rollback(ctx context.Context, req *pfkeeperv1.RollbackRequest) (*pfkeeperv1.RollbackResponse, error) {
	if s.shouldRedirect() {
		resp := &pfkeeperv1.RollbackResponse{}
		if err := s.redirect(ctx, rollbackM, req, resp); err != nil {
			s.audit(actorCN(ctx), rollbackM, map[string]string{"commit": req.GetCommit()}, "redirect failed")
			return nil, err
		}
		s.audit(actorCN(ctx), rollbackM, map[string]string{"commit": req.GetCommit()}, "redirected to master")
		return resp, nil
	}
	return s.rollbackLocal(ctx, actorCN(ctx), req.GetCommit())
}

func (s *Server) rollbackLocal(ctx context.Context, actor, commit string) (*pfkeeperv1.RollbackResponse, error) {
	if commit == "" {
		return nil, status.Error(codes.InvalidArgument, "commit is required")
	}
	if err := s.store.CheckoutCommit(ctx, commit); err != nil {
		s.audit(actor, rollbackM, map[string]string{"commit": commit}, "checkout failed: "+err.Error())
		return nil, status.Errorf(codes.Internal, "checkout %q: %v", commit, err)
	}
	st, err := state.LoadState(s.store.Dir)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load state: %v", err)
	}
	rev, err := s.store.HeadRevision(ctx)
	if err != nil {
		rev = commit
	}
	if _, err := s.rec.Reconcile(ctx, &st, rev, s.healthyMap()); err != nil {
		return nil, status.Errorf(codes.Internal, "reconcile: %v", err)
	}
	s.audit(actor, rollbackM, map[string]string{"commit": commit}, "ok revision="+rev)
	return &pfkeeperv1.RollbackResponse{Revision: rev}, nil
}
