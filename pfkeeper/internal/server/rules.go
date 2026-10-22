package server

import (
	"context"
	"os"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pfkeeperv1 "github.com/wsgoggway/lb-dsr-lab/pfkeeper/api/pfkeeperv1"
)

// GetAnchors returns the desired anchor fragments (rules/*.conf).
func (s *Server) GetAnchors(ctx context.Context, _ *emptypb.Empty) (*pfkeeperv1.GetAnchorsResponse, error) {
	anchors, err := s.store.Anchors()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read anchors: %v", err)
	}
	return &pfkeeperv1.GetAnchorsResponse{Anchors: anchors}, nil
}

// Validate syntax-checks a rules fragment as the pfkeeper anchor
// (`pfctl -a pfkeeper -n -f <tmp>`); nothing is applied.
func (s *Server) Validate(ctx context.Context, req *pfkeeperv1.ValidateRequest) (*pfkeeperv1.ValidateResponse, error) {
	tmp, err := os.CreateTemp("", "pfkeeper-validate-*.conf")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "temp file: %v", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
	if _, err := tmp.WriteString(req.GetRules()); err != nil {
		tmp.Close() //nolint:errcheck // best-effort
		return nil, status.Errorf(codes.Internal, "write temp file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, status.Errorf(codes.Internal, "close temp file: %v", err)
	}
	if err := s.pf.ValidateAnchor(ctx, "pfkeeper", tmpName); err != nil {
		return &pfkeeperv1.ValidateResponse{Ok: false, Error: err.Error()}, nil
	}
	return &pfkeeperv1.ValidateResponse{Ok: true}, nil
}

// ApplyMainConf validates conf/pf.conf and installs it unless dry_run.
func (s *Server) ApplyMainConf(ctx context.Context, req *pfkeeperv1.ApplyMainConfRequest) (*pfkeeperv1.ApplyMainConfResponse, error) {
	resp := &pfkeeperv1.ApplyMainConfResponse{}
	if err := s.rec.ApplyMainConf(ctx, req.GetDryRun()); err != nil {
		resp.Applied = false
		resp.ValidationError = err.Error()
		return resp, nil
	}
	dry := "false"
	if req.GetDryRun() {
		dry = "true"
	}
	s.audit(actorCN(ctx), "/pfkeeper.v1.RuleService/ApplyMainConf",
		map[string]string{"dry_run": dry}, "ok")
	resp.Applied = !req.GetDryRun()
	return resp, nil
}

// Diff reports desired-vs-live drift per table.
func (s *Server) Diff(ctx context.Context, _ *emptypb.Empty) (*pfkeeperv1.DiffResponse, error) {
	st, _, err := s.loadState()
	if err != nil {
		return nil, err
	}
	diffs, err := s.rec.DetectDrift(ctx, st, s.healthyMap())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "detect drift: %v", err)
	}
	out := make([]*pfkeeperv1.TableDiff, 0, len(diffs))
	diverged := false
	for _, d := range diffs {
		if len(d.Missing) > 0 || len(d.Extra) > 0 {
			diverged = true
		}
		out = append(out, &pfkeeperv1.TableDiff{
			Table:   d.Table,
			Missing: d.Missing,
			Extra:   d.Extra,
		})
	}
	return &pfkeeperv1.DiffResponse{Tables: out, Diverged: diverged}, nil
}
