package pfctl

import (
	"fmt"
	"strconv"
	"strings"
)

// PfInfo is a snapshot of pf runtime statistics (`pfctl -si`).
type PfInfo struct {
	StatesEntries  uint64 // "current entries" in the state table
	StatesSearches uint64 // state table searches
	StatesInserts  uint64 // state table inserts
	StatesRemovals uint64 // state table removals
	Match          uint64 // counters: match
	StateMismatch  uint64 // counters: state-mismatch
}

// TableMemberStats is one member of a table with its traffic counters
// (`pfctl -t <n> -T show -v`).
//
// InBlock/OutBlock keep the first value of the In/Block and Out/Block pairs
// (packet counters); the second value of each pair is dropped in v1.
type TableMemberStats struct {
	Address  string
	Cleared  string // free-form timestamp as printed by pfctl, "" if absent
	InBlock  uint64 // In/Block pair, first value (packets in)
	OutBlock uint64 // Out/Block pair, first value (packets out)
	XPass    uint64 // packets that bypassed the table evaluation
}

// siLabels maps `pfctl -si` line labels to a PfInfo field selector. Only
// known labels are collected; everything else in the output is ignored.
var siLabels = map[string]func(*PfInfo, uint64){
	"current entries": func(p *PfInfo, v uint64) { p.StatesEntries = v },
	"searches":        func(p *PfInfo, v uint64) { p.StatesSearches = v },
	"inserts":         func(p *PfInfo, v uint64) { p.StatesInserts = v },
	"removals":        func(p *PfInfo, v uint64) { p.StatesRemovals = v },
	"match":           func(p *PfInfo, v uint64) { p.Match = v },
	"state-mismatch":  func(p *PfInfo, v uint64) { p.StateMismatch = v },
}

// ParseSI parses `pfctl -si` output.
//
// Real lines look like:
//
//	current entries                     15
//	searches                         58451            1.9/s
//	match                             7169           22.2/s
//
// The label is the whitespace-separated token run before the first pure
// integer; the integer right after it is the total (rate columns ignored).
// An error is returned when the input contains no state table section.
func ParseSI(stdout string) (PfInfo, error) {
	var info PfInfo
	found := false
	for _, line := range strings.Split(stdout, "\n") {
		label, value, ok := parseSILine(line)
		if !ok {
			continue
		}
		if setter, known := siLabels[label]; known {
			found = found || label == "current entries"
			setter(&info, value)
		}
	}
	if !found {
		return PfInfo{}, fmt.Errorf("parse -si: no \"current entries\" line found")
	}
	return info, nil
}

// parseSILine splits one `-si` line into (label, total, ok). The label is the
// leading non-integer token run; the first pure integer is the total. Lines
// without any integer token yield ok=false.
func parseSILine(line string) (label string, value uint64, ok bool) {
	tokens := strings.Fields(line)
	i := 0
	for ; i < len(tokens); i++ {
		if v, err := strconv.ParseUint(tokens[i], 10, 64); err == nil {
			return strings.Join(tokens[:i], " "), v, i > 0
		}
	}
	return "", 0, false
}

// ParseTableShowV parses `pfctl -t <n> -T show -v` output. Real lines look
// like:
//
//	192.168.100.21, Cleared: Thu Oct 22 08:00:00 2026, In/Block: 0/0, Out/Block: 10/0, XPass: 0
//
// The address is the first field before the first comma; the trailing
// counters are optional (plain `-T show` lines carry the address only), and
// unparsable counter values are tolerated as zero.
func ParseTableShowV(stdout string) ([]TableMemberStats, error) {
	var res []TableMemberStats
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		member := TableMemberStats{}
		rest := line
		if idx := strings.Index(line, ","); idx >= 0 {
			member.Address = strings.TrimSpace(line[:idx])
			rest = line[idx+1:]
		} else {
			member.Address = line
			rest = ""
		}
		if member.Address == "" {
			continue
		}
		for _, part := range strings.Split(rest, ",") {
			part = strings.TrimSpace(part)
			switch {
			case strings.HasPrefix(part, "Cleared:"):
				member.Cleared = strings.TrimSpace(strings.TrimPrefix(part, "Cleared:"))
			case strings.HasPrefix(part, "In/Block:"):
				member.InBlock = parsePairFirst(strings.TrimPrefix(part, "In/Block:"))
			case strings.HasPrefix(part, "Out/Block:"):
				member.OutBlock = parsePairFirst(strings.TrimPrefix(part, "Out/Block:"))
			case strings.HasPrefix(part, "XPass:"):
				member.XPass, _ = strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(part, "XPass:")), 10, 64)
			}
		}
		res = append(res, member)
	}
	return res, nil
}

// parsePairFirst extracts the first integer of an "a/b" counter pair; 0 on
// any parse failure (tolerant by design).
func parsePairFirst(s string) uint64 {
	s = strings.TrimSpace(s)
	slash := strings.Index(s, "/")
	if slash >= 0 {
		s = s[:slash]
	}
	v, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	return v
}

// ParseRuleset parses `pfctl -sr` / `pfctl -a <n> -sr` output: one rule per
// line, trimmed, empty lines skipped.
func ParseRuleset(stdout string) ([]string, error) {
	lines := strings.Split(stdout, "\n")
	res := make([]string, 0, len(lines))
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			res = append(res, l)
		}
	}
	return res, nil
}
