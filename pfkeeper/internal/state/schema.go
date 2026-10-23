// Package state models the pfkeeper desired state stored in git and provides
// its on-disk layout, loading, validation and the git store over exec git.
//
// Directory layout of a state working tree (Dir):
//
//	config.yaml          — SyncConfig
//	tables/<name>.yaml   — one TableDef per file
//	services/<name>.yaml — one ServiceDef per file (v1.1 weighted balancing)
//	rules/<name>.conf    — pf anchor fragments, anchor name = file base name
//	conf/pf.conf         — main pf.conf (may be absent/empty)
package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// HealthExpect is the success criterion of an http health check.
type HealthExpect struct {
	Status []string `yaml:"status"` // codes/ranges: "200", "200-399", "2xx"; default 2xx
	JQ     string   `yaml:"jq"`     // gojq expression over the response body, optional
}

// UnmarshalYAML accepts both on-disk shapes of the expect block: the
// canonical flat form (status + jq) and the nested variant with body.jq
// seen on live stands (expect.body.jq). The nested jq folds into JQ when
// the flat one is empty; marshalling always emits the flat form.
func (e *HealthExpect) UnmarshalYAML(node *yaml.Node) error {
	var raw struct {
		Status []string `yaml:"status"`
		JQ     string   `yaml:"jq"`
		Body   *struct {
			JQ string `yaml:"jq"`
		} `yaml:"body"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	e.Status = raw.Status
	e.JQ = raw.JQ
	if e.JQ == "" && raw.Body != nil {
		e.JQ = raw.Body.JQ
	}
	return nil
}

// HealthSpec describes how to probe a table member.
type HealthSpec struct {
	Type     string       `yaml:"type"`     // "http" | "tcp" | "icmp"
	Port     int          `yaml:"port"`     // http/tcp target port
	Path     string       `yaml:"path"`     // http: request path
	Interval string       `yaml:"interval"` // e.g. "5s"
	Timeout  string       `yaml:"timeout"`  // e.g. "2s"
	Rise     int          `yaml:"rise"`     // consecutive passes -> UP
	Fall     int          `yaml:"fall"`     // consecutive failures -> DOWN
	Expect   HealthExpect `yaml:"expect"`   // http only
}

// DefaultMemberWeight is the distribution weight of a member without an
// explicit weight in the state (uniform share).
const DefaultMemberWeight = 1

// MemberDef is one table member: an IPv4 address and its traffic
// distribution weight. Weight 0 is a drain: the member stays in the table
// (health keeps probing it) but the weighted renderer gives it no rule.
type MemberDef struct {
	Addr   string `yaml:"addr"`
	Weight int    `yaml:"weight"` // 0..100; 0 = drain; default 1
}

// PlainMember builds a member with the default weight.
func PlainMember(addr string) MemberDef { return MemberDef{Addr: addr, Weight: DefaultMemberWeight} }

// PlainMembers builds members with the default weight.
func PlainMembers(addrs ...string) []MemberDef {
	out := make([]MemberDef, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, PlainMember(a))
	}
	return out
}

// Addrs returns the member addresses in order.
func Addrs(members []MemberDef) []string {
	out := make([]string, len(members))
	for i, m := range members {
		out[i] = m.Addr
	}
	return out
}

// TableDef is the desired state of one pf table.
type TableDef struct {
	Name        string      `yaml:"name"`
	Persist     bool        `yaml:"persist"`
	Members     []MemberDef `yaml:"members"`
	Description string      `yaml:"description"`
	Health      *HealthSpec `yaml:"health,omitempty"`
}

// UnmarshalYAML accepts both on-disk shapes of the members list: the legacy
// flat form (a list of address strings, weight = DefaultMemberWeight) and
// the structural form (a list of {addr, weight} maps, a missing weight key
// meaning DefaultMemberWeight — an explicit 0 stays a drain). Marshalling
// always emits the structural form (yaml field tags above).
func (td *TableDef) UnmarshalYAML(node *yaml.Node) error {
	var raw struct {
		Name        string      `yaml:"name"`
		Persist     bool        `yaml:"persist"`
		Members     yaml.Node   `yaml:"members"`
		Description string      `yaml:"description"`
		Health      *HealthSpec `yaml:"health"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	td.Name = raw.Name
	td.Persist = raw.Persist
	td.Description = raw.Description
	td.Health = raw.Health
	members, err := decodeMembers(&raw.Members)
	if err != nil {
		return err
	}
	td.Members = members
	return nil
}

// decodeMembers parses the members list in either shape.
func decodeMembers(node *yaml.Node) ([]MemberDef, error) {
	if node.Kind == 0 { // field absent
		return nil, nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("members: want a list, got %s", node.Tag)
	}
	out := make([]MemberDef, 0, len(node.Content))
	for _, item := range node.Content {
		switch item.Kind {
		case yaml.ScalarNode:
			out = append(out, PlainMember(item.Value))
		case yaml.MappingNode:
			var m struct {
				Addr   string `yaml:"addr"`
				Weight *int   `yaml:"weight"`
			}
			if err := item.Decode(&m); err != nil {
				return nil, fmt.Errorf("members: %w", err)
			}
			w := DefaultMemberWeight
			if m.Weight != nil {
				w = *m.Weight
			}
			out = append(out, MemberDef{Addr: m.Addr, Weight: w})
		default:
			return nil, fmt.Errorf("members: unsupported entry (%s)", item.Tag)
		}
	}
	return out, nil
}

// Service modes of a ServiceDef.
const (
	ModeWeighted   = "weighted"
	ModeRoundRobin = "round-robin"
)

// ListenDef is the DSR listen tuple of a service.
type ListenDef struct {
	Iface string `yaml:"iface"`
	VIP   string `yaml:"vip"`
	Port  int    `yaml:"port"`
	Proto string `yaml:"proto"` // "tcp" in v1
}

// ServiceDef is the desired state of one DSR service (services/<name>.yaml):
// which table supplies members and health, how traffic is distributed and
// where the service listens. The renderer turns it into the anchor
// pfkeeper/<service>.
type ServiceDef struct {
	Service string    `yaml:"service"`
	Table   string    `yaml:"table"`
	Mode    string    `yaml:"mode"` // weighted | round-robin; empty = round-robin
	Listen  ListenDef `yaml:"listen"`
	Sticky  bool      `yaml:"sticky"` // + sticky-address (per-source affinity)
}

// EffectiveMode returns Mode with the documented default applied (an absent
// mode keeps the legacy round-robin behaviour).
func (sd ServiceDef) EffectiveMode() string {
	if sd.Mode == "" {
		return ModeRoundRobin
	}
	return sd.Mode
}

// SyncConfig configures node-to-node synchronisation behaviour.
type SyncConfig struct {
	Mode            string `yaml:"mode"`             // "local" | "and" | "or"
	HealthConsensus string `yaml:"health_consensus"` // reserved (M4+)
	LagAlert        string `yaml:"lag_alert"`        // duration string, e.g. "30s"
	AutoHeal        *bool  `yaml:"auto_heal"`        // nil = default true
}

// Config is the root of config.yaml.
type Config struct {
	Sync SyncConfig `yaml:"sync"`
}

// State is everything loaded from one state working tree.
type State struct {
	Config   Config
	Tables   []TableDef
	Services []ServiceDef
	Anchors  map[string]string // anchor base name -> rules text (rules/*.conf)
	MainConf string            // conf/pf.conf content, "" when absent
}

// WriteTable marshals td to YAML and writes it to tables/<name>.yaml under
// dir (the desired-state mutation format used by TableService).
func WriteTable(dir string, td TableDef) error {
	b, err := yaml.Marshal(td)
	if err != nil {
		return fmt.Errorf("marshal table %q: %w", td.Name, err)
	}
	path := filepath.Join(dir, "tables", td.Name+".yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir tables: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// WriteService marshals sd to YAML and writes it to services/<name>.yaml
// under dir (the desired-state mutation format used by ServiceService and
// SetMemberWeight's table rewrite).
func WriteService(dir string, sd ServiceDef) error {
	b, err := yaml.Marshal(sd)
	if err != nil {
		return fmt.Errorf("marshal service %q: %w", sd.Service, err)
	}
	path := filepath.Join(dir, "services", sd.Service+".yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir services: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// LoadState reads and parses the whole state directory. Missing optional
// parts (no tables/, no services/, no rules/, no conf/pf.conf) are tolerated;
// a missing config.yaml yields the zero Config. Parse errors name the
// offending file. Tables, services and rule files are read in sorted order
// so the loaded state is deterministic.
func LoadState(dir string) (State, error) {
	st := State{Anchors: map[string]string{}}

	if b, err := os.ReadFile(filepath.Join(dir, "config.yaml")); err == nil {
		if err := yaml.Unmarshal(b, &st.Config); err != nil {
			return State{}, fmt.Errorf("parse config.yaml: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return State{}, fmt.Errorf("read config.yaml: %w", err)
	}

	tableFiles, err := filepath.Glob(filepath.Join(dir, "tables", "*.yaml"))
	if err != nil {
		return State{}, fmt.Errorf("glob tables: %w", err)
	}
	sort.Strings(tableFiles)
	for _, f := range tableFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			return State{}, fmt.Errorf("read %s: %w", f, err)
		}
		var td TableDef
		if err := yaml.Unmarshal(b, &td); err != nil {
			return State{}, fmt.Errorf("parse %s: %w", f, err)
		}
		st.Tables = append(st.Tables, td)
	}

	serviceFiles, err := filepath.Glob(filepath.Join(dir, "services", "*.yaml"))
	if err != nil {
		return State{}, fmt.Errorf("glob services: %w", err)
	}
	sort.Strings(serviceFiles)
	for _, f := range serviceFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			return State{}, fmt.Errorf("read %s: %w", f, err)
		}
		var sd ServiceDef
		if err := yaml.Unmarshal(b, &sd); err != nil {
			return State{}, fmt.Errorf("parse %s: %w", f, err)
		}
		st.Services = append(st.Services, sd)
	}

	ruleFiles, err := filepath.Glob(filepath.Join(dir, "rules", "*.conf"))
	if err != nil {
		return State{}, fmt.Errorf("glob rules: %w", err)
	}
	sort.Strings(ruleFiles)
	for _, f := range ruleFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			return State{}, fmt.Errorf("read %s: %w", f, err)
		}
		base := strings.TrimSuffix(filepath.Base(f), ".conf")
		st.Anchors[base] = string(b)
	}

	if b, err := os.ReadFile(filepath.Join(dir, "conf", "pf.conf")); err == nil {
		st.MainConf = string(b)
	} else if !os.IsNotExist(err) {
		return State{}, fmt.Errorf("read conf/pf.conf: %w", err)
	}

	return st, nil
}
