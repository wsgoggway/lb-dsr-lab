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
// member addresses (IPv4 only in v1) and weights, health specs (types,
// ports, durations, rise/fall, status matchers, jq compilation), services
// (names, modes, listen tuples, table references) and uniqueness of table,
// service and member names. It returns the first error found.
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
			ip := net.ParseIP(strings.TrimSpace(m.Addr))
			if ip == nil || ip.To4() == nil {
				return fmt.Errorf("table %q: member %q is not a valid IPv4 address", td.Name, m.Addr)
			}
			if seenMembers[m.Addr] {
				return fmt.Errorf("table %q: duplicate member %q", td.Name, m.Addr)
			}
			seenMembers[m.Addr] = true
			if m.Weight < 0 || m.Weight > 100 {
				return fmt.Errorf("table %q: member %q weight %d must be in 0..100 (0 = drain)", td.Name, m.Addr, m.Weight)
			}
		}

		if td.Health == nil {
			continue
		}
		if err := validateHealth(td.Health); err != nil {
			return fmt.Errorf("table %q health: %w", td.Name, err)
		}
	}
	return validateServices(st, seenTables)
}

// ifaceNameRe matches a pf interface name (ifconfig-style, no whitespace).
var ifaceNameRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,15}$`)

// validateServices checks every service: name shape and uniqueness, mode,
// listen tuple (iface/vip/port/proto), a resolvable table reference (which
// must not be shadowed by a rules/*.conf anchor) and, in weighted mode, at
// least one member with weight > 0.
func validateServices(st State, seenTables map[string]bool) error {
	seenServices := map[string]bool{}
	for _, sd := range st.Services {
		if !tableNameRe.MatchString(sd.Service) {
			return fmt.Errorf("service %q: name must match ^[a-zA-Z][a-zA-Z0-9_-]{0,31}$", sd.Service)
		}
		if seenServices[sd.Service] {
			return fmt.Errorf("duplicate service name %q", sd.Service)
		}
		seenServices[sd.Service] = true

		switch mode := sd.EffectiveMode(); mode {
		case ModeWeighted, ModeRoundRobin:
		default:
			return fmt.Errorf("service %q: mode %q must be one of %s|%s", sd.Service, sd.Mode, ModeWeighted, ModeRoundRobin)
		}

		l := sd.Listen
		if !ifaceNameRe.MatchString(l.Iface) {
			return fmt.Errorf("service %q: listen.iface %q is not a valid interface name", sd.Service, l.Iface)
		}
		if ip := net.ParseIP(strings.TrimSpace(l.VIP)); ip == nil || ip.To4() == nil {
			return fmt.Errorf("service %q: listen.vip %q is not a valid IPv4 address", sd.Service, l.VIP)
		}
		if l.Port < 1 || l.Port > 65535 {
			return fmt.Errorf("service %q: listen.port %d must be in 1..65535", sd.Service, l.Port)
		}
		if l.Proto != "tcp" {
			return fmt.Errorf("service %q: listen.proto %q must be \"tcp\" in v1", sd.Service, l.Proto)
		}

		if !seenTables[sd.Table] {
			return fmt.Errorf("service %q: table %q not found in the state", sd.Service, sd.Table)
		}
		if st.Anchors[sd.Service] != "" {
			return fmt.Errorf("service %q: name collides with rules anchor %q", sd.Service, sd.Service)
		}
		if sd.EffectiveMode() == ModeWeighted {
			td := tablesByName(st)[sd.Table]
			hasActive := false
			for _, m := range td.Members {
				if m.Weight > 0 {
					hasActive = true
					break
				}
			}
			if !hasActive {
				return fmt.Errorf("service %q: weighted mode requires table %q to have at least one member with weight > 0", sd.Service, sd.Table)
			}
		}
	}
	return nil
}

// tablesByName indexes the state's tables by name.
func tablesByName(st State) map[string]TableDef {
	out := make(map[string]TableDef, len(st.Tables))
	for _, td := range st.Tables {
		out[td.Name] = td
	}
	return out
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
