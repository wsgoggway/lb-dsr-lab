package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the pfkeeper.yaml daemon configuration.
type Config struct {
	Node         string `yaml:"node"`           // this node's name (CN of the node cert)
	PfctlBin     string `yaml:"pfctl_bin"`      // default /sbin/pfctl
	CarpIface    string `yaml:"carp_interface"` // interface carrying the carp: line
	CarpInterval string `yaml:"carp_interval"`  // carp poll interval, default 5s

	State struct {
		Dir    string `yaml:"dir"`    // working tree
		Remote string `yaml:"remote"` // origin URL (empty for single-node lab)
		Branch string `yaml:"branch"` // default main
		Key    string `yaml:"key"`    // deploy key for GIT_SSH_COMMAND
	} `yaml:"state"`

	Sync struct {
		Mode     string `yaml:"mode"`      // local|and|or, default local
		AutoHeal bool   `yaml:"auto_heal"` // divergence heal on MASTER
	} `yaml:"sync"`

	Listen struct {
		Client  string `yaml:"client"`  // client gRPC, default :8443
		Peer    string `yaml:"peer"`    // peer gRPC, default :8444
		Metrics string `yaml:"metrics"` // /metrics, default :9100
	} `yaml:"listen"`

	TLS struct {
		Cert string `yaml:"cert"`
		Key  string `yaml:"key"`
		CA   string `yaml:"ca"`
	} `yaml:"tls"`

	Peer struct {
		Endpoint string `yaml:"endpoint"` // peer gRPC endpoint, e.g. lb2:8444
	} `yaml:"peer"`
}

// LoadConfig reads and defaults the daemon config.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.defaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) defaults() {
	if c.PfctlBin == "" {
		c.PfctlBin = "/sbin/pfctl"
	}
	if c.State.Branch == "" {
		c.State.Branch = "main"
	}
	if c.Sync.Mode == "" {
		c.Sync.Mode = "local"
	}
	if c.Listen.Client == "" {
		c.Listen.Client = ":8443"
	}
	if c.Listen.Peer == "" {
		c.Listen.Peer = ":8444"
	}
	if c.Listen.Metrics == "" {
		c.Listen.Metrics = ":9100"
	}
	if c.CarpInterval == "" {
		c.CarpInterval = "5s"
	}
}

func (c *Config) validate() error {
	if c.Node == "" {
		return fmt.Errorf("config: node is required")
	}
	if c.State.Dir == "" {
		return fmt.Errorf("config: state.dir is required")
	}
	switch c.Sync.Mode {
	case "local", "and", "or":
	default:
		return fmt.Errorf("config: sync.mode must be local|and|or, got %q", c.Sync.Mode)
	}
	return nil
}
