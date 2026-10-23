// Package render turns (ServiceDef x TableDef x health view) into the
// deterministic pf anchor text of a service (docs/weighted-balancing.md).
//
// Weighted mode renders a probability chain: members with weight > 0 that
// are healthy, sorted by weight descending with the address as tie-break —
// the render must be byte-identical on both peers for a given view. The i-th
// rule matches with probability p_i = w_i / (w_i + ... + w_N) of the traffic
// still unmatched, in percent with one decimal (pfctl accepts fractions).
// The LAST rule of the chain is ALWAYS unconditional (catch-all, no
// probability): pf keeps evaluating rules past a probability miss even under
// `quick`, and without a closing unconditional rule the remaining
// probability leaks past the DSR routing (live PoC 2026-10-22: 7.3% of
// connections never reached a backend).
//
// Grammar: single-line legacy pf rules only (FreeBSD 14, no `\`
// continuations). Element order is fixed — `route-to (<if> <addr>)`
// immediately after `on <if>`; `sticky-address` (when enabled) right after
// the route-to pool; `probability P%` before `keep state`; `keep state
// (sloppy)` always last (DSR is asymmetric, sloppy states are mandatory).
//
// A member is excluded from the chain when its weight is 0 (drain) or it is
// not healthy; the weights of the remaining members renormalize through the
// formula. A weighted service with NO active member at all renders an EMPTY
// anchor and flags the plan Degraded: traffic toward the VIP is dropped
// instead of leaking past DSR — the honest failure mode when nothing behind
// the service is alive.
package render

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

// HealthyView is the health filter handed to the renderer:
// table -> set of UP addresses. An absent table key (or a nil view) means
// "no health opinion yet — everyone healthy" (the config-layer convention,
// so the renderer never fights the health layer on startup). It is an alias,
// interchangeable with reconcile.HealthyView.
type HealthyView = map[string]map[string]bool

// ErrNoActiveMembers reports a weighted service whose table currently has no
// member that is both weighted (weight > 0) and healthy.
var ErrNoActiveMembers = errors.New("no active members (weight > 0 and healthy)")

// MemberProbability is one chain entry of a rendered weighted service.
type MemberProbability struct {
	Addr        string
	Weight      int
	Probability float64 // share of the TOTAL traffic, percent (members sum to ~100)
}

// Plan is the rendered anchor of one service.
type Plan struct {
	Service string
	Anchor  string // anchor base name (= service name); applied as pfkeeper/<Anchor>
	Rules   string // anchor text; "" when the service has no active members
	// Degraded is set when the weighted service has no active member: the
	// anchor is rendered empty on purpose and the caller should alert.
	Degraded bool
	Members  []MemberProbability
}

// Service renders the anchor text for one service. healthy is the table's
// health set (nil = no opinion, everyone healthy). round-robin mode renders
// the legacy table-pool rule; weighted mode renders the probability chain.
func Service(sd state.ServiceDef, td state.TableDef, healthy map[string]bool) (Plan, error) {
	plan := Plan{Service: sd.Service, Anchor: sd.Service}
	switch mode := sd.EffectiveMode(); mode {
	case state.ModeRoundRobin:
		plan.Rules = roundRobinRule(sd)
		return plan, nil
	case state.ModeWeighted:
		return weighted(sd, td, healthy)
	default:
		return plan, fmt.Errorf("render service %q: unknown mode %q", sd.Service, sd.Mode)
	}
}

// All renders every service of st (in stored order; LoadState sorts) and
// merges the results into st.Anchors under the anchor name = service name —
// the same namespace the regular rules/*.conf anchors flow through. A
// service referencing a table missing from the state is an error. Plans are
// returned in service order.
func All(st *state.State, view HealthyView) ([]Plan, error) {
	tables := make(map[string]state.TableDef, len(st.Tables))
	for _, td := range st.Tables {
		tables[td.Name] = td
	}
	plans := make([]Plan, 0, len(st.Services))
	var errs []error
	for _, sd := range st.Services {
		td, ok := tables[sd.Table]
		if !ok {
			errs = append(errs, fmt.Errorf("render service %q: table %q not found in the state", sd.Service, sd.Table))
			continue
		}
		plan, err := Service(sd, td, view[sd.Table])
		if err != nil {
			if errors.Is(err, ErrNoActiveMembers) {
				plan.Degraded = true // empty anchor still merges below
			} else {
				errs = append(errs, err)
				continue
			}
		}
		if st.Anchors == nil {
			st.Anchors = map[string]string{}
		}
		st.Anchors[plan.Anchor] = plan.Rules
		plans = append(plans, plan)
	}
	return plans, errors.Join(errs...)
}

// weighted renders the probability chain (package doc). The LAST active
// member always gets the unconditional catch-all rule, whatever its weight.
func weighted(sd state.ServiceDef, td state.TableDef, healthy map[string]bool) (Plan, error) {
	active := make([]state.MemberDef, 0, len(td.Members))
	for _, m := range td.Members {
		if m.Weight > 0 && isHealthy(healthy, m.Addr) {
			active = append(active, m)
		}
	}
	// Deterministic order: weight descending, address ascending — both nodes
	// must derive byte-identical chains from the same view (peer-sync).
	sort.Slice(active, func(i, j int) bool {
		if active[i].Weight != active[j].Weight {
			return active[i].Weight > active[j].Weight
		}
		return active[i].Addr < active[j].Addr
	})

	plan := Plan{Service: sd.Service, Anchor: sd.Service}
	if len(active) == 0 {
		return plan, fmt.Errorf("render service %q: %w", sd.Service, ErrNoActiveMembers)
	}

	total := 0
	for _, m := range active {
		total += m.Weight
	}
	remainder := total

	var lines []string
	for i, m := range active {
		last := i == len(active)-1
		// Probability of the rule = share of the traffic still unmatched;
		// the published Probability = share of the total flow (the gauge).
		chainP := 100 * float64(m.Weight) / float64(remainder)
		shareP := 100 * float64(m.Weight) / float64(total)
		plan.Members = append(plan.Members, MemberProbability{
			Addr:        m.Addr,
			Weight:      m.Weight,
			Probability: round1(shareP),
		})
		prob := ""
		if !last {
			prob = formatPercent(chainP)
		}
		lines = append(lines, ruleLine(sd, m.Addr, prob))
		remainder -= m.Weight
	}
	plan.Rules = strings.Join(lines, "\n") + "\n"
	return plan, nil
}

// isHealthy applies the health set: a nil set means no health opinion
// (healthy); a present set vouches only for its members.
func isHealthy(healthy map[string]bool, addr string) bool {
	if healthy == nil {
		return true
	}
	return healthy[addr]
}

// ruleLine renders one single-line legacy pf rule; an empty probability
// renders the unconditional (catch-all) form. keep state is always last.
func ruleLine(sd state.ServiceDef, addr, probability string) string {
	var b strings.Builder
	b.WriteString("pass in quick on ")
	b.WriteString(sd.Listen.Iface)
	b.WriteString(" route-to (")
	b.WriteString(sd.Listen.Iface)
	b.WriteString(" ")
	b.WriteString(addr)
	b.WriteString(")")
	if sd.Sticky {
		b.WriteString(" sticky-address")
	}
	b.WriteString(" proto ")
	b.WriteString(sd.Listen.Proto)
	b.WriteString(" to ")
	b.WriteString(sd.Listen.VIP)
	b.WriteString(" port ")
	b.WriteString(strconv.Itoa(sd.Listen.Port))
	if probability != "" {
		b.WriteString(" probability ")
		b.WriteString(probability)
		b.WriteString("%")
	}
	b.WriteString(" keep state (sloppy)")
	return b.String()
}

// roundRobinRule renders the v1 legacy pool rule: equal shares over the pf
// table with per-source affinity (sticky-address, as on the current stand).
func roundRobinRule(sd state.ServiceDef) string {
	var b strings.Builder
	b.WriteString("pass in quick on ")
	b.WriteString(sd.Listen.Iface)
	b.WriteString(" route-to (")
	b.WriteString(sd.Listen.Iface)
	b.WriteString(" <")
	b.WriteString(sd.Table)
	b.WriteString(">) round-robin sticky-address")
	b.WriteString(" proto ")
	b.WriteString(sd.Listen.Proto)
	b.WriteString(" to ")
	b.WriteString(sd.Listen.VIP)
	b.WriteString(" port ")
	b.WriteString(strconv.Itoa(sd.Listen.Port))
	b.WriteString(" keep state (sloppy)")
	b.WriteString("\n")
	return b.String()
}

// formatPercent renders a chain probability with one decimal (pfctl accepts
// fractions; "50.0%" is valid).
func formatPercent(p float64) string {
	return strconv.FormatFloat(round1(p), 'f', 1, 64)
}

// round1 rounds to one decimal place (half away from zero).
func round1(p float64) float64 {
	return math.Round(p*10) / 10
}
