package pfctl

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Counter is one named pf statistic line (label run before the first integer
// plus the integer itself). Rate columns of `pfctl -si` are dropped.
type Counter struct {
	Label string
	Value uint64
}

// Stats is a snapshot of `pfctl -si` output.
type Stats struct {
	// States is the "current entries" value: the live size of the pf state
	// table. This is a gauge (point-in-time number), NOT a monotonic counter.
	States uint64
	// StateTable holds the monotonic counters of the "State Table" section
	// (searches, inserts, removals, ...).
	StateTable []Counter
	// Counters holds the monotonic counters of the "Counters" section
	// (match, bad-offset, state-mismatch, ...).
	Counters []Counter
}

// ParseSI parses `pfctl -si` output into a Stats snapshot. Everything before
// the "State Table" header (the Status/Debug banner) is ignored; each section
// line yields a Counter whose label is the whitespace-separated token run
// before the first pure integer (trailing rate columns are dropped).
//
// Real FreeBSD 14 lines look like:
//
//	State Table                          Total             Rate
//	  current entries                       16
//	  searches                         1541072           11.7/s
//	Counters
//	  match                            116163            0.9/s
//
// An error is returned when the input contains no "current entries" line.
func ParseSI(stdout string) (Stats, error) {
	var stats Stats
	section := 0 // 0 = preamble, 1 = State Table, 2 = Counters
	found := false
	for _, line := range strings.Split(stdout, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "State Table"):
			section = 1
			continue
		case trimmed == "Counters":
			section = 2
			continue
		}
		if section == 0 {
			continue
		}
		label, value, ok := parseLabeledInt(line)
		if !ok {
			continue
		}
		if section == 1 {
			if label == "current entries" {
				stats.States = value
				found = true
				continue
			}
			stats.StateTable = append(stats.StateTable, Counter{Label: label, Value: value})
			continue
		}
		stats.Counters = append(stats.Counters, Counter{Label: label, Value: value})
	}
	if !found {
		return Stats{}, fmt.Errorf("parse -si: no %q line found", "current entries")
	}
	return stats, nil
}

// parseLabeledInt splits one pfctl stat line into (label, first integer, ok).
// The label is the whitespace-separated token run before the first pure
// integer; further integers on the line (rate columns) are ignored. Lines
// without a label before the first integer yield ok=false.
func parseLabeledInt(line string) (label string, value uint64, ok bool) {
	tokens := strings.Fields(line)
	for i, tok := range tokens {
		v, err := strconv.ParseUint(tok, 10, 64)
		if err != nil {
			continue
		}
		if i == 0 {
			return "", 0, false
		}
		return strings.Join(tokens[:i], " "), v, true
	}
	return "", 0, false
}

// InterfacePackets carries one per-interface traffic counter from the verbose
// `pfctl -sI -v` block.
type InterfacePackets struct {
	Family    string // "4" | "6"
	Direction string // "in" | "out"
	Action    string // "pass" | "block"
	Packets   uint64
	Bytes     uint64
}

// Interface is one interface listed by `pfctl -sI`.
type Interface struct {
	Name string
	// Skip is true when pf skips evaluation on the interface
	// ("set skip on", printed as a "(skip)" marker by FreeBSD).
	Skip bool
	// Packets carries the per-direction counters. Empty on FreeBSD 14.x,
	// whose pfctl lists interface names only (the OpenBSD-style verbose
	// blocks are parsed where present).
	Packets []InterfacePackets
}

// ifacePacketsRe matches one OpenBSD-style verbose counter line, e.g.
//
//	In4/Pass:    [ Packets: 123                Bytes: 456                  ]
var ifacePacketsRe = regexp.MustCompile(`^(In|Out)(4|6)/(Pass|Block):\s*\[\s*Packets:\s*(\d+)\s+Bytes:\s*(\d+)`)

// ParseInterfaces parses `pfctl -sI -v` output. A non-indented line starts a
// new interface (first token = name, "(skip)" marks skipped evaluation);
// indented lines carry optional verbose counters, parsed when they match the
// "In4/Pass: [ Packets: N Bytes: M ]" shape and tolerated otherwise.
func ParseInterfaces(stdout string) []Interface {
	var res []Interface
	var cur *Interface
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if line[0] != '\t' && line[0] != ' ' {
			fields := strings.Fields(line)
			skip := false
			for _, f := range fields[1:] {
				if f == "(skip)" {
					skip = true
				}
			}
			res = append(res, Interface{Name: fields[0], Skip: skip})
			cur = &res[len(res)-1]
			continue
		}
		m := ifacePacketsRe.FindStringSubmatch(strings.TrimLeft(line, " \t"))
		if m == nil || cur == nil {
			continue
		}
		packets, _ := strconv.ParseUint(m[4], 10, 64)
		bytesCount, _ := strconv.ParseUint(m[5], 10, 64)
		cur.Packets = append(cur.Packets, InterfacePackets{
			Family:    m[2],
			Direction: strings.ToLower(m[1]),
			Action:    strings.ToLower(m[3]),
			Packets:   packets,
			Bytes:     bytesCount,
		})
	}
	return res
}

// ParseTables parses plain `pfctl -sT` output: one table name per line,
// empty lines skipped.
func ParseTables(stdout string) []string {
	var res []string
	for _, line := range strings.Split(stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			res = append(res, line)
		}
	}
	return res
}

// ParseTableShow counts the member addresses in `pfctl -t <name> -T show`
// output (one address per line, empty lines ignored).
func ParseTableShow(stdout string) int {
	n := 0
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}
