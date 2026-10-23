package collector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// stubRunner answers pfctl invocations from a fixture map keyed by the args
// joined with spaces; non-nil err fails every invocation.
type stubRunner struct {
	responses map[string]string
	err       error
}

func (s stubRunner) Run(_ context.Context, _ string, args ...string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	key := strings.Join(args, " ")
	out, ok := s.responses[key]
	if !ok {
		return "", fmt.Errorf("stubRunner: unexpected argv %q", key)
	}
	return out, nil
}

// failingAfterSI answers only "-si" and fails every other invocation.
type failingAfterSI struct{ ok string }

func (f failingAfterSI) Run(_ context.Context, _ string, args ...string) (string, error) {
	if strings.Join(args, " ") == "-si" {
		return f.ok, nil
	}
	return "", errors.New("forced failure")
}

func newTestCollector(t *testing.T, runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}) *Collector {
	t.Helper()
	return New("/sbin/pfctl", runner)
}

// fixtureDir points at the module-root testdata shared with the pfctl
// package tests: one canonical set of captured pfctl outputs.
const fixtureDir = "../../testdata"

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(fixtureDir + "/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(data)
}

// gather scrapes the collector and renders every sample as
// `name{label=value,...} value` so identically named series with different
// labels stay distinct.
func gather(t *testing.T, c *Collector) map[string]float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register collector: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	m := make(map[string]float64)
	for _, mf := range families {
		for _, metric := range mf.GetMetric() {
			name := mf.GetName()
			if labels := metricLabels(metric); labels != "" {
				name += "{" + labels + "}"
			}
			switch {
			case metric.GetCounter() != nil:
				m[name] = metric.GetCounter().GetValue()
			case metric.GetGauge() != nil:
				m[name] = metric.GetGauge().GetValue()
			}
		}
	}
	return m
}

func metricLabels(m *dto.Metric) string {
	parts := make([]string, 0, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		parts = append(parts, lp.GetName()+"="+lp.GetValue())
	}
	return strings.Join(parts, ",")
}

func TestCollector_FreeBSDScrape(t *testing.T) {
	t.Parallel()

	runner := stubRunner{responses: map[string]string{
		"-si":             readFixture(t, "pfctl_si_freebsd.txt"),
		"-sI -v":          readFixture(t, "pfctl_sI_freebsd.txt"),
		"-sT":             "webs\n",
		"-t webs -T show": "192.168.100.21\n192.168.100.22\n",
	}}
	m := gather(t, newTestCollector(t, runner))

	// Gauge: state table size.
	if v := m["pf_states"]; v != 20 {
		t.Errorf("pf_states = %v, want 20", v)
	}
	// State Table section counters keep the pf_states_ prefix.
	if v := m["pf_states_searches_total"]; v != 1541595 {
		t.Errorf("pf_states_searches_total = %v, want 1541595", v)
	}
	if v := m["pf_states_removals_total"]; v != 75687 {
		t.Errorf("pf_states_removals_total = %v, want 75687", v)
	}
	// Counters section: dashes slugified.
	if v := m["pf_match_total"]; v != 116280 {
		t.Errorf("pf_match_total = %v, want 116280", v)
	}
	if v := m["pf_state_mismatch_total"]; v != 9 {
		t.Errorf("pf_state_mismatch_total = %v, want 9", v)
	}
	if v := m["pf_bad_offset_total"]; v != 0 {
		t.Errorf("pf_bad_offset_total = %v, want 0", v)
	}
	// Interfaces: one info gauge per listed interface, no packet counters on
	// FreeBSD.
	if v := m["pf_interface{interface=vtnet0,skip=false}"]; v != 1 {
		t.Errorf("pf_interface{vtnet0} = %v, want 1", v)
	}
	if v := m["pf_interface{interface=lo0,skip=true}"]; v != 1 {
		t.Errorf("pf_interface{lo0,skip} = %v, want 1", v)
	}
	if _, ok := m["pf_interface_packets_total"]; ok {
		t.Error("pf_interface_packets_total must be absent on FreeBSD -sI output")
	}
	// Tables.
	if v := m["pf_table_addresses{table=webs}"]; v != 2 {
		t.Errorf("pf_table_addresses{webs} = %v, want 2", v)
	}
	// No failures recorded on the happy path.
	if _, ok := m["pf_pfctl_errors_total"]; ok {
		t.Error("pf_pfctl_errors_total must be absent when every pfctl call succeeds")
	}
}

func TestCollector_OpenBSDStyleInterfaceCounters(t *testing.T) {
	t.Parallel()

	runner := stubRunner{responses: map[string]string{
		"-si":    readFixture(t, "pfctl_si.txt"),
		"-sI -v": readFixture(t, "pfctl_siv.txt"),
		"-sT":    "",
	}}
	m := gather(t, newTestCollector(t, runner))

	// The OpenBSD-style verbose block: In4/Pass of interface "all".
	key := "pf_interface_packets_total{action=pass,direction=in,family=4,interface=all}"
	if v, ok := m[key]; !ok || v != 0 {
		t.Errorf("%s = %v (present=%v), want 0", key, v, ok)
	}
}

func TestCollector_PfctlFailureRecordsError(t *testing.T) {
	t.Parallel()

	runner := stubRunner{err: errors.New("exit status 1: pfctl: /dev/pf: No such file or directory")}
	m := gather(t, newTestCollector(t, runner))

	if v := m["pf_pfctl_errors_total{command=si}"]; v != 1 {
		t.Errorf("pf_pfctl_errors_total{si} = %v, want 1", v)
	}
	if v := m["pf_pfctl_errors_total{command=sI}"]; v != 1 {
		t.Errorf("pf_pfctl_errors_total{sI} = %v, want 1", v)
	}
	if _, ok := m["pf_states"]; ok {
		t.Error("pf_states must be absent when pfctl fails")
	}
}

func TestCollector_PartialFailureIsolatesFamilies(t *testing.T) {
	t.Parallel()

	// -si works, everything else fails: only the info family survives.
	c := newTestCollector(t, failingAfterSI{ok: readFixture(t, "pfctl_si_freebsd.txt")})
	m := gather(t, c)

	if v := m["pf_states"]; v != 20 {
		t.Errorf("pf_states = %v, want 20", v)
	}
	if _, ok := m["pf_interface"]; ok {
		t.Error("pf_interface must be absent when -sI fails")
	}
	if _, ok := m["pf_table_addresses"]; ok {
		t.Error("pf_table_addresses must be absent when -sT fails")
	}
	if v := m["pf_pfctl_errors_total{command=sI}"]; v != 1 {
		t.Errorf("pf_pfctl_errors_total{sI} = %v, want 1", v)
	}
}

func TestSlug(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, want string
	}{
		{"match", "match"},
		{"bad-offset", "bad_offset"},
		{"ip-option", "ip_option"},
		{"proto-cksum", "proto_cksum"},
		{"state-mismatch", "state_mismatch"},
		{"map-failed", "map_failed"},
		{"weird--multi  spaces", "weird_multi_spaces"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			if got := slug(tt.in); got != tt.want {
				t.Errorf("slug(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
