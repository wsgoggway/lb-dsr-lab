// Package metrics owns the pfkeeper Prometheus registry: pf state, member
// health, probes, applies, drift, peering, git sync, CARP role and host
// system gauges.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics holds every pfkeeper metric registered on a private registry.
type Metrics struct {
	reg *prometheus.Registry

	PfStatesEntries  prometheus.Gauge
	PfStatesSearches prometheus.Gauge
	PfStatesInserts  prometheus.Gauge
	PfStatesRemovals prometheus.Gauge
	PfMatch          prometheus.Gauge
	PfStateMismatch  prometheus.Gauge

	// MemberHealth: 1 = UP, 0 = DOWN/INACTIVE/unknown.
	MemberHealth *prometheus.GaugeVec
	// ProbeFailures counts failed probes by machine reason.
	ProbeFailures *prometheus.CounterVec
	// ProbeTotal counts probes by result (ok|fail).
	ProbeTotal *prometheus.CounterVec
	// ServiceProbability carries the rendered traffic share (percent) of
	// each member of a weighted service; republished on every render.
	ServiceProbability *prometheus.GaugeVec

	// ApplyTotal counts reconcile passes by result (ok|error).
	ApplyTotal *prometheus.CounterVec
	// Drift is 1 while desired != live (last DetectDrift/Reconcile view).
	Drift prometheus.Gauge

	PeerReachable  prometheus.Gauge
	PeerDiverged   prometheus.Gauge
	PeerLagSeconds prometheus.Gauge

	// GitPullTotal counts state pulls by result (ok|error|skipped).
	GitPullTotal *prometheus.CounterVec

	// CarpRole carries 1 on the active role label, 0 on the others.
	CarpRole *prometheus.GaugeVec

	MemUsedBytes      prometheus.Gauge
	MemAvailableBytes prometheus.Gauge
	MemUsedPercent    prometheus.Gauge
	Load1             prometheus.Gauge
	Load5             prometheus.Gauge
	Load15            prometheus.Gauge
	CPUPercent        prometheus.Gauge
}

// New builds and registers all metrics on a fresh (non-default) registry.
func New(node string) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := &Metrics{reg: reg}

	gauge := func(name, help string) prometheus.Gauge {
		g := prometheus.NewGauge(prometheus.GaugeOpts{Namespace: "pfkeeper", Name: name, Help: help, ConstLabels: prometheus.Labels{"node": node}})
		reg.MustRegister(g)
		return g
	}
	gaugeVec := func(name, help string, labels []string) *prometheus.GaugeVec {
		v := prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: "pfkeeper", Name: name, Help: help, ConstLabels: prometheus.Labels{"node": node}}, labels)
		reg.MustRegister(v)
		return v
	}
	counterVec := func(name, help string, labels []string) *prometheus.CounterVec {
		v := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "pfkeeper", Name: name, Help: help, ConstLabels: prometheus.Labels{"node": node}}, labels)
		reg.MustRegister(v)
		return v
	}

	m.PfStatesEntries = gauge("pf_states_entries", "Current pf state table entries.")
	m.PfStatesSearches = gauge("pf_states_searches", "State table searches (cumulative, from pfctl -si).")
	m.PfStatesInserts = gauge("pf_states_inserts", "State table inserts (cumulative).")
	m.PfStatesRemovals = gauge("pf_states_removals", "State table removals (cumulative).")
	m.PfMatch = gauge("pf_match", "pf match counter (cumulative).")
	m.PfStateMismatch = gauge("pf_state_mismatch", "pf state-mismatch counter (cumulative).")

	m.MemberHealth = gaugeVec("member_health", "Member health: 1 UP, 0 otherwise.", []string{"table", "address"})
	m.ProbeFailures = counterVec("probe_failures_total", "Failed health probes by reason.", []string{"table", "address", "reason"})
	m.ProbeTotal = counterVec("probe_total", "Health probes by result.", []string{"table", "address", "result"})

	m.ServiceProbability = gaugeVec("service_probability", "Rendered traffic share (percent) of a weighted service member.", []string{"service", "member"})

	m.ApplyTotal = counterVec("apply_total", "Reconcile passes by result.", []string{"result"})
	m.Drift = gauge("drift", "1 when the desired state diverges from live pf.")

	m.PeerReachable = gauge("peer_reachable", "1 when the peer gRPC is reachable.")
	m.PeerDiverged = gauge("peer_diverged", "1 when the last SyncCheck reported divergence.")
	m.PeerLagSeconds = gauge("peer_lag_seconds", "Seconds since the peer was last seen reachable (-1: never).")

	m.GitPullTotal = counterVec("git_pull_total", "Git state pulls by result.", []string{"result"})

	m.CarpRole = gaugeVec("carp_role", "CARP role of the monitored interface (1 = active).", []string{"role"})

	m.MemUsedBytes = gauge("sys_mem_used_bytes", "Used memory (gopsutil).")
	m.MemAvailableBytes = gauge("sys_mem_available_bytes", "Available memory (gopsutil).")
	m.MemUsedPercent = gauge("sys_mem_used_percent", "Memory used percent.")
	m.Load1 = gauge("sys_load1", "1-minute load average.")
	m.Load5 = gauge("sys_load5", "5-minute load average.")
	m.Load15 = gauge("sys_load15", "15-minute load average.")
	m.CPUPercent = gauge("sys_cpu_percent", "Total CPU busy percent.")
	return m
}

// Registry exposes the prometheus registry for promhttp.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// SetPf publishes one `pfctl -si` snapshot.
func (m *Metrics) SetPf(entries, searches, inserts, removals, match, mismatch uint64) {
	m.PfStatesEntries.Set(float64(entries))
	m.PfStatesSearches.Set(float64(searches))
	m.PfStatesInserts.Set(float64(inserts))
	m.PfStatesRemovals.Set(float64(removals))
	m.PfMatch.Set(float64(match))
	m.PfStateMismatch.Set(float64(mismatch))
}

// SetMemberHealth records a monitor state: 1 for UP, 0 otherwise.
func (m *Metrics) SetMemberHealth(table, addr, state string) {
	v := 0.0
	if state == "UP" {
		v = 1
	}
	m.MemberHealth.WithLabelValues(table, addr).Set(v)
}

// ObserveProbe folds a probe outcome into ProbeTotal/ProbeFailures.
func (m *Metrics) ObserveProbe(table, addr string, passed bool, reason string) {
	result := "ok"
	if !passed {
		result = "fail"
		m.ProbeFailures.WithLabelValues(table, addr, reason).Inc()
	}
	m.ProbeTotal.WithLabelValues(table, addr, result).Inc()
}

// SetServiceProbability publishes one rendered member share (percent).
func (m *Metrics) SetServiceProbability(service, member string, pct float64) {
	m.ServiceProbability.WithLabelValues(service, member).Set(pct)
}

// ResetServicePlans drops every service probability series. Called before a
// full republish so drained or removed members do not linger on the gauge.
func (m *Metrics) ResetServicePlans() { m.ServiceProbability.Reset() }

// IncApply counts one reconcile pass.
func (m *Metrics) IncApply(err bool) {
	result := "ok"
	if err {
		result = "error"
	}
	m.ApplyTotal.WithLabelValues(result).Inc()
}

// SetDrift records the drift flag.
func (m *Metrics) SetDrift(drift bool) {
	v := 0.0
	if drift {
		v = 1
	}
	m.Drift.Set(v)
}

// SetPeer publishes the peering observation.
func (m *Metrics) SetPeer(reachable, diverged bool, lagSeconds int64) {
	rv, dv := 0.0, 0.0
	if reachable {
		rv = 1
	}
	if diverged {
		dv = 1
	}
	m.PeerReachable.Set(rv)
	m.PeerDiverged.Set(dv)
	m.PeerLagSeconds.Set(float64(lagSeconds))
}

// IncGitPull counts one git watcher pass.
func (m *Metrics) IncGitPull(result string) { m.GitPullTotal.WithLabelValues(result).Inc() }

// SetCarpRole sets 1 on the active role and 0 on all others.
func (m *Metrics) SetCarpRole(role string) {
	for _, r := range []string{"MASTER", "BACKUP", "INIT"} {
		v := 0.0
		if r == role {
			v = 1
		}
		m.CarpRole.WithLabelValues(r).Set(v)
	}
}
