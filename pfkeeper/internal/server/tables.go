package server

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	pfkeeperv1 "github.com/wsgoggway/lb-dsr-lab/pfkeeper/api/pfkeeperv1"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/health"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

const (
	tableSvc         = "/pfkeeper.v1.TableService/"
	setTableM        = tableSvc + "SetTable"
	addMembersM      = tableSvc + "AddMembers"
	deleteMembersM   = tableSvc + "DeleteMembers"
	setMemberWeightM = tableSvc + "SetMemberWeight"
)

// tableToProto converts a desired-state table into the wire form.
func tableToProto(td state.TableDef) *pfkeeperv1.Table {
	t := &pfkeeperv1.Table{
		Name:        td.Name,
		Persist:     td.Persist,
		Members:     state.Addrs(td.Members),
		Description: td.Description,
	}
	for _, m := range td.Members {
		t.MembersDetailed = append(t.MembersDetailed, &pfkeeperv1.Member{Addr: m.Addr, Weight: int32(m.Weight)})
	}
	if td.Health != nil {
		t.Health = &pfkeeperv1.HealthSpec{
			Type:     td.Health.Type,
			Port:     int32(td.Health.Port),
			Path:     td.Health.Path,
			Interval: td.Health.Interval,
			Timeout:  td.Health.Timeout,
			Rise:     int32(td.Health.Rise),
			Fall:     int32(td.Health.Fall),
			Expect: &pfkeeperv1.HealthSpec_Expect{
				Status: td.Health.Expect.Status,
				Jq:     td.Health.Expect.JQ,
			},
		}
	}
	return t
}

// ListTables returns the desired tables from the git state.
func (s *Server) ListTables(ctx context.Context, _ *emptypb.Empty) (*pfkeeperv1.ListTablesResponse, error) {
	st, _, err := s.loadState()
	if err != nil {
		return nil, err
	}
	tables := make([]*pfkeeperv1.Table, 0, len(st.Tables))
	for _, td := range st.Tables {
		tables = append(tables, tableToProto(td))
	}
	return &pfkeeperv1.ListTablesResponse{Tables: tables}, nil
}

// GetTable returns desired state, live pf members and the applied revision.
func (s *Server) GetTable(ctx context.Context, req *pfkeeperv1.GetTableRequest) (*pfkeeperv1.GetTableResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "table name is required")
	}
	st, rev, err := s.loadState()
	if err != nil {
		return nil, err
	}
	for _, td := range st.Tables {
		if td.Name != req.GetName() {
			continue
		}
		effective, err := s.pf.TableShow(ctx, td.Name)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "table show %q: %v", td.Name, err)
		}
		return &pfkeeperv1.GetTableResponse{
			Table:     tableToProto(td),
			Effective: effective,
			Revision:  rev,
		}, nil
	}
	return nil, status.Errorf(codes.NotFound, "table %q not found in the state", req.GetName())
}

// SetTable replaces the desired member set of a table, commits and
// reconciles. On a BACKUP with a reachable peer the mutation is proxied.
func (s *Server) SetTable(ctx context.Context, req *pfkeeperv1.SetTableRequest) (*pfkeeperv1.SetTableResponse, error) {
	if s.shouldRedirect() {
		resp := &pfkeeperv1.SetTableResponse{}
		if err := s.redirect(ctx, setTableM, req, resp); err != nil {
			s.audit(actorCN(ctx), setTableM, paramsOf(req.GetName(), req.GetReason()), "redirect failed")
			return nil, err
		}
		s.audit(actorCN(ctx), setTableM, paramsOf(req.GetName(), req.GetReason()), "redirected to master")
		return resp, nil
	}
	return s.setTableLocal(ctx, actorCN(ctx), req)
}

func (s *Server) setTableLocal(ctx context.Context, actor string, req *pfkeeperv1.SetTableRequest) (*pfkeeperv1.SetTableResponse, error) {
	params := paramsOf(req.GetName(), req.GetReason())
	// SetTable replaces the member SET: everyone enters with the default
	// weight (use SetMemberWeight to adjust weights afterwards).
	commit, st, err := s.mutateTable(ctx, actor, req.GetName(), func([]state.MemberDef) ([]state.MemberDef, error) {
		return state.PlainMembers(req.GetMembers()...), nil
	}, req.GetReason(), setTableM)
	if err != nil {
		return nil, err
	}
	if _, err := s.rec.Reconcile(ctx, st, commit, s.healthyMap()); err != nil {
		s.audit(actor, setTableM, params, "reconcile failed: "+err.Error())
		return nil, status.Errorf(codes.Internal, "reconcile: %v", err)
	}
	s.audit(actor, setTableM, params, "ok commit="+commit)
	return &pfkeeperv1.SetTableResponse{Commit: commit}, nil
}

// AddMembers adds members to a table's desired set.
func (s *Server) AddMembers(ctx context.Context, req *pfkeeperv1.AddMembersRequest) (*pfkeeperv1.AddMembersResponse, error) {
	if s.shouldRedirect() {
		resp := &pfkeeperv1.AddMembersResponse{}
		if err := s.redirect(ctx, addMembersM, req, resp); err != nil {
			s.audit(actorCN(ctx), addMembersM, paramsOf(req.GetTable(), req.GetReason()), "redirect failed")
			return nil, err
		}
		s.audit(actorCN(ctx), addMembersM, paramsOf(req.GetTable(), req.GetReason()), "redirected to master")
		return resp, nil
	}
	return s.addMembersLocal(ctx, actorCN(ctx), req)
}

func (s *Server) addMembersLocal(ctx context.Context, actor string, req *pfkeeperv1.AddMembersRequest) (*pfkeeperv1.AddMembersResponse, error) {
	commit, st, err := s.mutateTable(ctx, actor, req.GetTable(), func(members []state.MemberDef) ([]state.MemberDef, error) {
		out := append([]state.MemberDef(nil), members...)
		for _, m := range req.GetMembers() {
			if !slices.Contains(state.Addrs(out), m) {
				out = append(out, state.PlainMember(m))
			}
		}
		return out, nil
	}, req.GetReason(), addMembersM)
	if err != nil {
		return nil, err
	}
	if _, err := s.rec.Reconcile(ctx, st, commit, s.healthyMap()); err != nil {
		return nil, status.Errorf(codes.Internal, "reconcile: %v", err)
	}
	s.audit(actor, addMembersM, paramsOf(req.GetTable(), req.GetReason()), "ok commit="+commit)
	return &pfkeeperv1.AddMembersResponse{Commit: commit}, nil
}

// DeleteMembers removes members from a table's desired set.
func (s *Server) DeleteMembers(ctx context.Context, req *pfkeeperv1.DeleteMembersRequest) (*pfkeeperv1.DeleteMembersResponse, error) {
	if s.shouldRedirect() {
		resp := &pfkeeperv1.DeleteMembersResponse{}
		if err := s.redirect(ctx, deleteMembersM, req, resp); err != nil {
			s.audit(actorCN(ctx), deleteMembersM, paramsOf(req.GetTable(), req.GetReason()), "redirect failed")
			return nil, err
		}
		s.audit(actorCN(ctx), deleteMembersM, paramsOf(req.GetTable(), req.GetReason()), "redirected to master")
		return resp, nil
	}
	return s.deleteMembersLocal(ctx, actorCN(ctx), req)
}

func (s *Server) deleteMembersLocal(ctx context.Context, actor string, req *pfkeeperv1.DeleteMembersRequest) (*pfkeeperv1.DeleteMembersResponse, error) {
	commit, st, err := s.mutateTable(ctx, actor, req.GetTable(), func(members []state.MemberDef) ([]state.MemberDef, error) {
		out := make([]state.MemberDef, 0, len(members))
		for _, m := range members {
			if !slices.Contains(req.GetMembers(), m.Addr) {
				out = append(out, m)
			}
		}
		return out, nil
	}, req.GetReason(), deleteMembersM)
	if err != nil {
		return nil, err
	}
	if _, err := s.rec.Reconcile(ctx, st, commit, s.healthyMap()); err != nil {
		return nil, status.Errorf(codes.Internal, "reconcile: %v", err)
	}
	s.audit(actor, deleteMembersM, paramsOf(req.GetTable(), req.GetReason()), "ok commit="+commit)
	return &pfkeeperv1.DeleteMembersResponse{Commit: commit}, nil
}

// SetMemberWeight sets the distribution weight of one member (0 = drain)
// through the regular mutation pipeline: git commit with the caller CN as
// actor, then reconcile (which re-renders the weighted service chains).
// On a BACKUP with a reachable peer the mutation is proxied to the MASTER.
func (s *Server) SetMemberWeight(ctx context.Context, req *pfkeeperv1.SetMemberWeightRequest) (*pfkeeperv1.SetMemberWeightResponse, error) {
	if s.shouldRedirect() {
		resp := &pfkeeperv1.SetMemberWeightResponse{}
		if err := s.redirect(ctx, setMemberWeightM, req, resp); err != nil {
			s.audit(actorCN(ctx), setMemberWeightM, weightParams(req), "redirect failed")
			return nil, err
		}
		s.audit(actorCN(ctx), setMemberWeightM, weightParams(req), "redirected to master")
		return resp, nil
	}
	return s.setMemberWeightLocal(ctx, actorCN(ctx), req)
}

func (s *Server) setMemberWeightLocal(ctx context.Context, actor string, req *pfkeeperv1.SetMemberWeightRequest) (*pfkeeperv1.SetMemberWeightResponse, error) {
	params := weightParams(req)
	addr := strings.TrimSpace(req.GetAddress())
	switch {
	case req.GetTable() == "":
		return nil, status.Error(codes.InvalidArgument, "table name is required")
	case addr == "":
		return nil, status.Error(codes.InvalidArgument, "member address is required")
	case req.GetWeight() < 0 || req.GetWeight() > 100:
		return nil, status.Errorf(codes.InvalidArgument, "weight %d must be in 0..100 (0 = drain)", req.GetWeight())
	}
	commit, st, err := s.mutateTable(ctx, actor, req.GetTable(), func(members []state.MemberDef) ([]state.MemberDef, error) {
		out := make([]state.MemberDef, 0, len(members))
		found := false
		for _, m := range members {
			if m.Addr == addr {
				m.Weight = int(req.GetWeight())
				found = true
			}
			out = append(out, m)
		}
		if !found {
			return nil, fmt.Errorf("member %q not found in table %q", addr, req.GetTable())
		}
		return out, nil
	}, req.GetReason(), setMemberWeightM)
	if err != nil {
		return nil, err
	}
	if _, err := s.rec.Reconcile(ctx, st, commit, s.healthyMap()); err != nil {
		s.audit(actor, setMemberWeightM, params, "reconcile failed: "+err.Error())
		return nil, status.Errorf(codes.Internal, "reconcile: %v", err)
	}
	s.audit(actor, setMemberWeightM, params, "ok commit="+commit)
	return &pfkeeperv1.SetMemberWeightResponse{Commit: commit}, nil
}

// weightParams builds the audit params of a weight mutation.
func weightParams(req *pfkeeperv1.SetMemberWeightRequest) map[string]string {
	p := paramsOf(req.GetTable(), req.GetReason())
	p["address"] = req.GetAddress()
	p["weight"] = fmt.Sprintf("%d", req.GetWeight())
	return p
}

// TestAddress checks live pf table membership.
func (s *Server) TestAddress(ctx context.Context, req *pfkeeperv1.TestAddressRequest) (*pfkeeperv1.TestAddressResponse, error) {
	found, err := s.pf.TableTest(ctx, req.GetTable(), req.GetAddress())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "table test: %v", err)
	}
	return &pfkeeperv1.TestAddressResponse{Found: found}, nil
}

// mutateTable applies mutFn to the table's desired members (addresses with
// weights), validates the whole state, writes tables/<name>.yaml and
// commits+pushes as actor.
func (s *Server) mutateTable(ctx context.Context, actor, name string, mutFn func([]state.MemberDef) ([]state.MemberDef, error), reason, method string) (string, *state.State, error) {
	params := paramsOf(name, reason)
	if name == "" {
		return "", nil, status.Error(codes.InvalidArgument, "table name is required")
	}
	st, _, err := s.loadState()
	if err != nil {
		return "", nil, err
	}
	idx := slices.IndexFunc(st.Tables, func(td state.TableDef) bool { return td.Name == name })
	var members []state.MemberDef
	if idx >= 0 {
		members = st.Tables[idx].Members
	}
	health := (*state.HealthSpec)(nil)
	if idx >= 0 {
		health = st.Tables[idx].Health
	}
	newMembers, err := mutFn(members)
	if err != nil {
		s.audit(actor, method, params, "rejected: "+err.Error())
		return "", nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}

	td := state.TableDef{Name: name, Persist: idx >= 0 && st.Tables[idx].Persist, Members: newMembers, Health: health}
	if idx >= 0 {
		td.Description = st.Tables[idx].Description
	}
	newTables := append([]state.TableDef(nil), st.Tables...)
	if idx >= 0 {
		newTables[idx] = td
	} else {
		newTables = append(newTables, td)
	}
	newState := &state.State{Config: st.Config, Tables: newTables, Services: st.Services, Anchors: st.Anchors, MainConf: st.MainConf}
	if err := state.ValidateState(*newState); err != nil {
		s.audit(actor, method, params, "rejected: "+err.Error())
		return "", nil, status.Errorf(codes.InvalidArgument, "validation: %v", err)
	}
	if err := state.WriteTable(s.store.Dir, td); err != nil {
		return "", nil, status.Errorf(codes.Internal, "write table: %v", err)
	}
	msg := fmt.Sprintf("%s %s", methodShort(method), name)
	if reason != "" {
		msg += ": " + reason
	}
	commit, err := s.store.CommitAndPush(ctx, actor, msg, "tables/"+name+".yaml")
	if err != nil {
		s.audit(actor, method, params, "commit failed: "+err.Error())
		return "", nil, status.Errorf(codes.Internal, "commit and push: %v", err)
	}
	return commit, newState, nil
}

// memberToProto renders a monitor's latest state for the wire.
func memberToProto(m *health.Monitor) *pfkeeperv1.MemberHealth {
	last := m.Last()
	mh := &pfkeeperv1.MemberHealth{
		Table:       m.Table,
		Address:     m.Address,
		State:       m.State(),
		StatusCode:  int32(last.StatusCode),
		BodyExcerpt: last.BodyExcerpt,
		JqOutput:    last.JQOutput,
		Reason:      last.Reason,
	}
	if !last.CheckedAt.IsZero() {
		mh.CheckedAt = timestamppb.New(last.CheckedAt)
	}
	return mh
}

func paramsOf(name, reason string) map[string]string {
	p := map[string]string{"table": name}
	if reason != "" {
		p["reason"] = reason
	}
	return p
}

// methodShort turns "/pfkeeper.v1.TableService/SetTable" into "set table".
func methodShort(method string) string {
	switch {
	case strings.HasSuffix(method, "SetTable"):
		return "set table"
	case strings.HasSuffix(method, "AddMembers"):
		return "add members to"
	case strings.HasSuffix(method, "DeleteMembers"):
		return "delete members from"
	case strings.HasSuffix(method, "SetMemberWeight"):
		return "set weight in"
	case strings.HasSuffix(method, "Sync"):
		return "sync"
	case strings.HasSuffix(method, "Rollback"):
		return "rollback"
	default:
		return method
	}
}
