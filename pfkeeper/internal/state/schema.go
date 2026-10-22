// Package state models the pfkeeper desired state stored in git and provides
// its on-disk layout, loading, validation and the git store over exec git.
//
// Directory layout of a state working tree (Dir):
//
//	config.yaml        — SyncConfig
//	tables/<name>.yaml — one TableDef per file
//	rules/<name>.conf  — pf anchor fragments, anchor name = file base name
//	conf/pf.conf       — main pf.conf (may be absent/empty)
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

// TableDef is the desired state of one pf table.
type TableDef struct {
	Name        string      `yaml:"name"`
	Persist     bool        `yaml:"persist"`
	Members     []string    `yaml:"members"`
	Description string      `yaml:"description"`
	Health      *HealthSpec `yaml:"health,omitempty"`
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

// LoadState reads and parses the whole state directory. Missing optional
// parts (no tables/, no rules/, no conf/pf.conf) are tolerated; a missing
// config.yaml yields the zero Config. Parse errors name the offending file.
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
