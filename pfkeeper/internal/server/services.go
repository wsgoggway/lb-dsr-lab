package server

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pfkeeperv1 "github.com/wsgoggway/lb-dsr-lab/pfkeeper/api/pfkeeperv1"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/render"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

const (
	serviceSvc    = "/pfkeeper.v1.ServiceService/"
	listServicesM = serviceSvc + "ListServices"
	getServiceM   = serviceSvc + "GetService"
)

// serviceToProto converts a desired-state service into the wire form.
func serviceToProto(sd state.ServiceDef) *pfkeeperv1.ServiceDef {
	return &pfkeeperv1.ServiceDef{
		Service: sd.Service,
		Table:   sd.Table,
		Mode:    sd.EffectiveMode(),
		Listen: &pfkeeperv1.ServiceDef_Listen{
			Iface: sd.Listen.Iface,
			Vip:   sd.Listen.VIP,
			Port:  int32(sd.Listen.Port),
			Proto: sd.Listen.Proto,
		},
		Sticky: sd.Sticky,
	}
}

// ListServices returns the desired services from the git state.
func (s *Server) ListServices(ctx context.Context, _ *emptypb.Empty) (*pfkeeperv1.ListServicesResponse, error) {
	st, _, err := s.loadState()
	if err != nil {
		return nil, err
	}
	services := make([]*pfkeeperv1.ServiceDef, 0, len(st.Services))
	for _, sd := range st.Services {
		services = append(services, serviceToProto(sd))
	}
	return &pfkeeperv1.ListServicesResponse{Services: services}, nil
}

// GetService returns the desired service together with its rendered chain:
// healthy members, weights and effective probabilities (weight-descending
// order, the catch-all member carries its effective share). The render uses
// the same health view as the reconciler.
func (s *Server) GetService(ctx context.Context, req *pfkeeperv1.GetServiceRequest) (*pfkeeperv1.GetServiceResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "service name is required")
	}
	st, rev, err := s.loadState()
	if err != nil {
		return nil, err
	}
	for _, sd := range st.Services {
		if sd.Service != req.GetName() {
			continue
		}
		td, ok := tableByName(st, sd.Table)
		if !ok {
			return nil, status.Errorf(codes.Internal, "service %q references missing table %q", sd.Service, sd.Table)
		}
		healthyTable := s.healthyMap()[sd.Table]
		plan, err := render.Service(sd, td, healthyTable)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "render service %q: %v", sd.Service, err)
		}
		resp := &pfkeeperv1.GetServiceResponse{
			Service:  serviceToProto(sd),
			Members:  make([]*pfkeeperv1.ServiceMember, 0, len(td.Members)),
			Anchor:   plan.Rules,
			Revision: rev,
		}
		if sd.EffectiveMode() == state.ModeWeighted {
			for _, mp := range plan.Members {
				resp.Members = append(resp.Members, &pfkeeperv1.ServiceMember{
					Addr:        mp.Addr,
					Weight:      int32(mp.Weight),
					Healthy:     isMemberHealthy(healthyTable, mp.Addr),
					Probability: mp.Probability,
				})
			}
			return resp, nil
		}
		// round-robin: distribution is not expressed per member; report the
		// desired members with their weights and health only.
		for _, m := range td.Members {
			resp.Members = append(resp.Members, &pfkeeperv1.ServiceMember{
				Addr:    m.Addr,
				Weight:  int32(m.Weight),
				Healthy: isMemberHealthy(healthyTable, m.Addr),
			})
		}
		return resp, nil
	}
	return nil, status.Errorf(codes.NotFound, "service %q not found in the state", req.GetName())
}

// tableByName finds a table definition in the state.
func tableByName(st *state.State, name string) (state.TableDef, bool) {
	for _, td := range st.Tables {
		if td.Name == name {
			return td, true
		}
	}
	return state.TableDef{}, false
}

// isMemberHealthy applies the table's health set (nil set = no opinion).
func isMemberHealthy(healthy map[string]bool, addr string) bool {
	if healthy == nil {
		return true
	}
	return healthy[addr]
}
