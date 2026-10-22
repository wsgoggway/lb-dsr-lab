package state

import (
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/itchyny/gojq"
)

var (
	tableNameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,31}$`)
	// statusRe matches "200", "200-299" and "2xx" (case-insensitive).
	statusRe = regexp.MustCompile(`(?i)^\d+$|^\d+-\d+$|^\dxx$`)
)

// ValidateState checks a loaded State for semantic errors: table names,
// member addresses (IPv4 only in v1), health specs (types, ports, durations,
// rise/fall, status matchers, jq compilation) and uniqueness of table names
// and members. It returns the first error found.
func ValidateState(st State) error {
	seenTables := map[string]bool{}
	for _, td := range st.Tables {
		if !tableNameRe.MatchString(td.Name) {
			return fmt.Errorf("table %q: name must match ^[a-zA-Z][a-zA-Z0-9_-]{0,31}$", td.Name)
		}
		if seenTables[td.Name] {
			return fmt.Errorf("duplicate table name %q", td.Name)
		}
		seenTables[td.Name] = true

		seenMembers := map[string]bool{}
		for _, m := range td.Members {
			ip := net.ParseIP(strings.TrimSpace(m))
			if ip == nil || ip.To4() == nil {
				return fmt.Errorf("table %q: member %q is not a valid IPv4 address", td.Name, m)
			}
			if seenMembers[m] {
				return fmt.Errorf("table %q: duplicate member %q", td.Name, m)
			}
			seenMembers[m] = true
		}

		if td.Health == nil {
			continue
		}
		if err := validateHealth(td.Health); err != nil {
			return fmt.Errorf("table %q health: %w", td.Name, err)
		}
	}
	return nil
}

// healthTypes is the set of supported prober types in v1.
var healthTypes = map[string]bool{"http": true, "tcp": true, "icmp": true}

func validateHealth(h *HealthSpec) error {
	if !healthTypes[h.Type] {
		return fmt.Errorf("type %q must be one of http|tcp|icmp", h.Type)
	}
	if h.Type == "http" && h.Port <= 0 {
		return fmt.Errorf("http health requires port > 0, got %d", h.Port)
	}
	for name, v := range map[string]string{"interval": h.Interval, "timeout": h.Timeout} {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%s %q: %w", name, v, err)
		}
		if d <= 0 {
			return fmt.Errorf("%s must be > 0, got %q", name, v)
		}
	}
	if h.Rise < 1 {
		return fmt.Errorf("rise must be >= 1, got %d", h.Rise)
	}
	if h.Fall < 1 {
		return fmt.Errorf("fall must be >= 1, got %d", h.Fall)
	}
	for _, s := range h.Expect.Status {
		if !statusRe.MatchString(s) {
			return fmt.Errorf("expect.status entry %q must match ^\\d+$|^\\d+-\\d+$|^\\dxx$", s)
		}
	}
	if h.Expect.JQ != "" {
		if _, err := gojq.Parse(h.Expect.JQ); err != nil {
			return fmt.Errorf("expect.jq %q: %w", h.Expect.JQ, err)
		}
	}
	return nil
}
