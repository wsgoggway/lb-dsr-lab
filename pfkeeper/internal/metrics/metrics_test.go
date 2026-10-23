package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestCountersAndGaugesChange(t *testing.T) {
	m := New("test-node")

	// pf gauges.
	m.SetPf(15, 58451, 12548, 200, 7169, 90)
	if got := testutil.ToFloat64(m.PfStatesEntries); got != 15 {
		t.Fatalf("pf_states_entries = %v, want 15", got)
	}
	if got := testutil.ToFloat64(m.PfMatch); got != 7169 {
		t.Fatalf("pf_match = %v, want 7169", got)
	}
	m.SetPf(20, 58500, 12550, 210, 7200, 91)
	if got := testutil.ToFloat64(m.PfStatesEntries); got != 20 {
		t.Fatalf("pf_states_entries after update = %v, want 20", got)
	}

	// member health gauge.
	m.SetMemberHealth("web", "10.0.0.1", "UP")
	if got := testutil.ToFloat64(m.MemberHealth.WithLabelValues("web", "10.0.0.1")); got != 1 {
		t.Fatalf("member_health UP = %v, want 1", got)
	}
	m.SetMemberHealth("web", "10.0.0.1", "DOWN")
	if got := testutil.ToFloat64(m.MemberHealth.WithLabelValues("web", "10.0.0.1")); got != 0 {
		t.Fatalf("member_health DOWN = %v, want 0", got)
	}

	// probe counters.
	m.ObserveProbe("web", "10.0.0.1", true, "")
	m.ObserveProbe("web", "10.0.0.2", false, "timeout")
	m.ObserveProbe("web", "10.0.0.2", false, "timeout")
	if got := testutil.ToFloat64(m.ProbeTotal.WithLabelValues("web", "10.0.0.1", "ok")); got != 1 {
		t.Fatalf("probe_total ok = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.ProbeFailures.WithLabelValues("web", "10.0.0.2", "timeout")); got != 2 {
		t.Fatalf("probe_failures timeout = %v, want 2", got)
	}

	// apply + drift.
	m.IncApply(false)
	m.IncApply(true)
	if got := testutil.ToFloat64(m.ApplyTotal.WithLabelValues("ok")); got != 1 {
		t.Fatalf("apply_total ok = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.ApplyTotal.WithLabelValues("error")); got != 1 {
		t.Fatalf("apply_total error = %v, want 1", got)
	}
	m.SetDrift(true)
	if got := testutil.ToFloat64(m.Drift); got != 1 {
		t.Fatalf("drift = %v, want 1", got)
	}

	// peer.
	m.SetPeer(true, false, 0)
	if got := testutil.ToFloat64(m.PeerReachable); got != 1 {
		t.Fatalf("peer_reachable = %v, want 1", got)
	}
	m.SetPeer(false, true, 42)
	if got := testutil.ToFloat64(m.PeerLagSeconds); got != 42 {
		t.Fatalf("peer_lag_seconds = %v, want 42", got)
	}

	// service probability gauge (v1.1): published per render, reset drops
	// drained members.
	m.SetServiceProbability("web", "192.168.100.21", 50)
	m.SetServiceProbability("web", "192.168.100.22", 30)
	if got := testutil.ToFloat64(m.ServiceProbability.WithLabelValues("web", "192.168.100.21")); got != 50 {
		t.Fatalf("service_probability = %v, want 50", got)
	}
	m.ResetServicePlans()
	m.SetServiceProbability("web", "192.168.100.22", 100)
	if got := testutil.ToFloat64(m.ServiceProbability.WithLabelValues("web", "192.168.100.22")); got != 100 {
		t.Fatalf("service_probability after reset = %v, want 100", got)
	}

	// git + carp.
	m.IncGitPull("ok")
	if got := testutil.ToFloat64(m.GitPullTotal.WithLabelValues("ok")); got != 1 {
		t.Fatalf("git_pull_total ok = %v, want 1", got)
	}
	m.SetCarpRole("MASTER")
	if got := testutil.ToFloat64(m.CarpRole.WithLabelValues("MASTER")); got != 1 {
		t.Fatalf("carp_role MASTER = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.CarpRole.WithLabelValues("BACKUP")); got != 0 {
		t.Fatalf("carp_role BACKUP = %v, want 0", got)
	}

	// Node label present on gathered families.
	mfs, err := m.reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	found := false
	for _, mf := range mfs {
		if mf.GetName() == "pfkeeper_drift" && len(mf.GetMetric()) == 1 {
			lbl := mf.GetMetric()[0].GetLabel()
			if len(lbl) == 1 && lbl[0].GetValue() == "test-node" && mf.GetMetric()[0].GetGauge().GetValue() == 1 {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("pfkeeper_drift{node=test-node}=1 not found in gathered metrics")
	}
}
