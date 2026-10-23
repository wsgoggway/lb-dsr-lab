// Package collector turns live pfctl(8) output into Prometheus metrics.
// pfctl is exec'd on every scrape: metrics always reflect the live ruleset,
// there is no caching layer.
package collector

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/wsgoggway/lb-dsr-lab/exporters/pf-exporter/internal/pfctl"
)

// scrapeTimeout bounds each individual pfctl invocation performed by Collect.
const scrapeTimeout = 5 * time.Second

// Collector implements prometheus.Collector by shelling out to pfctl(8).
type Collector struct {
	pfctlPath string
	runner    pfctl.Runner

	errMu       sync.Mutex
	pfctlErrors map[string]uint64 // per-subcommand invocation failure count
}

// New builds a Collector that execs pfctlPath. A non-nil runner overrides the
// executor (tests).
func New(pfctlPath string, runner pfctl.Runner) *Collector {
	if runner == nil {
		runner = pfctl.ExecRunner{}
	}
	return &Collector{
		pfctlPath:   pfctlPath,
		runner:      runner,
		pfctlErrors: map[string]uint64{},
	}
}

// Describe sends no descriptors: metric names are derived from pfctl output
// (generic counter labels), so this is an unchecked collector by design.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {}

// Collect scrapes pfctl and emits the metric families below. A failing or
// unparsable subcommand skips only its own metrics and increments
// pf_pfctl_errors_total{command}.
//
//	pf_states                       gauge    state table "current entries"
//	pf_states_<label>_total         counter  -si "State Table" section
//	pf_<label>_total                counter  -si "Counters" section
//	pf_interface{interface,skip}    gauge    -sI -v listing (1 per interface)
//	pf_interface_packets_total{interface,family,direction,action} counter
//	pf_table_addresses{table}       gauge    member count per -sT table
//	pf_pfctl_errors_total{command}  counter  pfctl invocation failures
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()

	c.collectInfo(ctx, ch)
	c.collectInterfaces(ctx, ch)
	c.collectTables(ctx, ch)
	c.collectErrors(ch)
}

func (c *Collector) collectInfo(ctx context.Context, ch chan<- prometheus.Metric) {
	out, err := c.run(ctx, "si", "-si")
	if err != nil {
		return
	}
	stats, err := pfctl.ParseSI(out)
	if err != nil {
		slog.Warn("pf_exporter: parse -si failed", "err", err)
		c.recordError("si")
		return
	}
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("pf_states",
			"Current number of entries in the pf state table (pfctl -si current entries).",
			nil, nil),
		prometheus.GaugeValue, float64(stats.States))
	for _, ctr := range stats.StateTable {
		ch <- counterMetric("pf_states_"+slug(ctr.Label)+"_total",
			"pf -si State Table counter "+ctr.Label+" (monotonic).", ctr.Value)
	}
	for _, ctr := range stats.Counters {
		ch <- counterMetric("pf_"+slug(ctr.Label)+"_total",
			"pf -si Counters section: "+ctr.Label+" (monotonic).", ctr.Value)
	}
}

func (c *Collector) collectInterfaces(ctx context.Context, ch chan<- prometheus.Metric) {
	out, err := c.run(ctx, "sI", "-sI", "-v")
	if err != nil {
		return
	}
	for _, iface := range pfctl.ParseInterfaces(out) {
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc("pf_interface",
				"pf interface known to pfctl -sI (1 = listed).",
				[]string{"interface", "skip"}, nil),
			prometheus.GaugeValue, 1, iface.Name, strconv.FormatBool(iface.Skip))
		for _, p := range iface.Packets {
			ch <- prometheus.MustNewConstMetric(
				prometheus.NewDesc("pf_interface_packets_total",
					"pf per-interface packets (pfctl -sI -v verbose block, monotonic).",
					[]string{"interface", "family", "direction", "action"}, nil),
				prometheus.CounterValue, float64(p.Packets),
				iface.Name, p.Family, p.Direction, p.Action)
		}
	}
}

func (c *Collector) collectTables(ctx context.Context, ch chan<- prometheus.Metric) {
	out, err := c.run(ctx, "sT", "-sT")
	if err != nil {
		return
	}
	for _, table := range pfctl.ParseTables(out) {
		members, err := c.run(ctx, "table_show", "-t", table, "-T", "show")
		if err != nil {
			continue
		}
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc("pf_table_addresses",
				"Number of member addresses in a pf table (pfctl -sT + -T show).",
				[]string{"table"}, nil),
			prometheus.GaugeValue, float64(pfctl.ParseTableShow(members)), table)
	}
}

func (c *Collector) collectErrors(ch chan<- prometheus.Metric) {
	c.errMu.Lock()
	snap := make(map[string]uint64, len(c.pfctlErrors))
	for k, v := range c.pfctlErrors {
		snap[k] = v
	}
	c.errMu.Unlock()
	for cmd, count := range snap {
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc("pf_pfctl_errors_total",
				"Failed pfctl invocations by subcommand (monotonic).",
				[]string{"command"}, nil),
			prometheus.CounterValue, float64(count), cmd)
	}
}

// run execs one pfctl subcommand; failures are logged and counted, and the
// error is returned so the caller can skip its metric family.
func (c *Collector) run(ctx context.Context, subcmd string, args ...string) (string, error) {
	out, err := c.runner.Run(ctx, c.pfctlPath, args...)
	if err != nil {
		c.recordError(subcmd)
		slog.Warn("pf_exporter: pfctl failed", "subcommand", subcmd, "err", err)
		return "", err
	}
	return out, nil
}

func (c *Collector) recordError(subcmd string) {
	c.errMu.Lock()
	c.pfctlErrors[subcmd]++
	c.errMu.Unlock()
}

// counterMetric builds an unchecked counter with a dynamic name.
func counterMetric(name, help string, value uint64) prometheus.Metric {
	return prometheus.MustNewConstMetric(
		prometheus.NewDesc(name, help, nil, nil),
		prometheus.CounterValue, float64(value))
}

// slug converts a pfctl label into a Prometheus name fragment: lowercase,
// every non-alphanumeric run collapsed to a single underscore
// ("bad-offset" -> "bad_offset", "proto cksum" -> "proto_cksum").
func slug(label string) string {
	var b strings.Builder
	b.Grow(len(label))
	prevUnderscore := true // suppress a leading underscore
	for _, r := range strings.ToLower(label) {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		switch {
		case isAlnum:
			b.WriteRune(r)
			prevUnderscore = false
		case !prevUnderscore:
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}
