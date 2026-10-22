package pfctl

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// CarpState is the observed CARP status of one interface.
type CarpState struct {
	Role      string // "MASTER" | "BACKUP" | "INIT"
	Vhid      string
	Interface string
}

// CarpRole runs `ifconfig <iface>` and parses the carp: line.
// Requires root (the daemon runs privileged); returns Role INIT when the
// interface carries no carp: line.
func CarpRole(ctx context.Context, iface string) (CarpState, error) {
	var runner Runner = ExecRunner{}
	out, err := runner.Run(ctx, "/sbin/ifconfig", iface)
	if err != nil {
		return CarpState{}, fmt.Errorf("ifconfig %s: %w", iface, err)
	}
	st := ParseCarp(iface, out)
	return st, nil
}

// ParseCarp extracts role and vhid from `ifconfig <iface>` output. Real line:
//
//	carp: MASTER vhid 1 advbase 1 advskew 0
//
// Missing line -> Role "INIT"; unknown role token is passed through as-is so
// the daemon surfaces surprises instead of hiding them.
func ParseCarp(iface, ifconfigOut string) CarpState {
	st := CarpState{Role: "INIT", Interface: iface}
	for _, line := range strings.Split(ifconfigOut, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 || fields[0] != "carp:" {
			continue
		}
		st.Role = strings.ToUpper(fields[1])
		for i := 2; i+1 < len(fields); i += 2 {
			if fields[i] == "vhid" {
				if _, err := strconv.Atoi(fields[i+1]); err == nil {
					st.Vhid = fields[i+1]
				}
			}
		}
		break
	}
	return st
}
