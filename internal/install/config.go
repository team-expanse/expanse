// Package install implements the expanse installer: config parsing,
// hardware detection, identity and the staged install plan.
package install

import (
	"fmt"
	"net"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// CurrentConfigVersion is the only supported install config version.
const CurrentConfigVersion = 1

// DiskLayout selects the ZFS topology.
type DiskLayout string

const (
	LayoutAuto   DiskLayout = "auto"
	LayoutSingle DiskLayout = "single"
	LayoutMirror DiskLayout = "mirror"
	LayoutRaidz1 DiskLayout = "raidz1"
)

// EncryptionConfig configures ZFS native encryption.
type EncryptionConfig struct {
	Enabled bool `yaml:"enabled"`
}

// DisksConfig configures target disks and layout.
type DisksConfig struct {
	Layout     DiskLayout       `yaml:"layout"`
	Devices    []string         `yaml:"devices"`
	Force      bool             `yaml:"force"`
	Encryption EncryptionConfig `yaml:"encryption"`
}

// StaticNetworkConfig holds a static address assignment.
type StaticNetworkConfig struct {
	Address string   `yaml:"address"`
	Gateway string   `yaml:"gateway"`
	DNS     []string `yaml:"dns"`
}

// NetworkConfig configures the node's network.
type NetworkConfig struct {
	Interface string              `yaml:"interface"`
	Mode      string              `yaml:"mode"` // dhcp | static
	Static    StaticNetworkConfig `yaml:"static"`
}

// ClusterConfig configures cluster bootstrap; wired up in Phase 03.
type ClusterConfig struct {
	Mode        string `yaml:"mode"` // none | init | join
	JoinAddress string `yaml:"join_address"`
	JoinToken   string `yaml:"join_token"`
}

// SSHConfig holds operator access keys.
type SSHConfig struct {
	AuthorizedKeys []string `yaml:"authorized_keys"`
}

// Config is the parsed expanse-install.yaml.
type Config struct {
	Version  int           `yaml:"version"`
	Hostname string        `yaml:"hostname"`
	Disks    DisksConfig   `yaml:"disks"`
	Network  NetworkConfig `yaml:"network"`
	Cluster  ClusterConfig `yaml:"cluster"`
	SSH      SSHConfig     `yaml:"ssh"`
	Timezone string        `yaml:"timezone"`
}

// DefaultConfig returns the built-in defaults.
func DefaultConfig() *Config {
	return &Config{
		Version:  CurrentConfigVersion,
		Disks:    DisksConfig{Layout: LayoutAuto},
		Network:  NetworkConfig{Interface: "auto", Mode: "dhcp"},
		Cluster:  ClusterConfig{Mode: "none"},
		Timezone: "UTC",
	}
}

// LoadConfig reads and validates an install config from path.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read install config: %w", err)
	}
	return ParseConfig(data)
}

// ParseConfig parses and validates install config YAML.
func ParseConfig(data []byte) (*Config, error) {
	c := DefaultConfig()
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse install config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid install config: %w", err)
	}
	return c, nil
}

// Validate checks the config for structural errors.
func (c *Config) Validate() error {
	if c.Version != CurrentConfigVersion {
		return fmt.Errorf("unknown version %d (supported: %d)", c.Version, CurrentConfigVersion)
	}
	switch c.Disks.Layout {
	case LayoutAuto, LayoutSingle, LayoutMirror, LayoutRaidz1:
	default:
		return fmt.Errorf("disks.layout: unknown layout %q", c.Disks.Layout)
	}
	switch c.Network.Mode {
	case "dhcp", "static":
	default:
		return fmt.Errorf("network.mode: must be dhcp or static, got %q", c.Network.Mode)
	}
	if c.Network.Mode == "static" {
		if c.Network.Static.Address == "" {
			return fmt.Errorf("network.static.address: required when mode is static")
		}
		if _, _, err := net.ParseCIDR(c.Network.Static.Address); err != nil {
			return fmt.Errorf("network.static.address: invalid CIDR %q", c.Network.Static.Address)
		}
		if c.Network.Static.Gateway == "" {
			return fmt.Errorf("network.static.gateway: required when mode is static")
		}
		if net.ParseIP(c.Network.Static.Gateway) == nil {
			return fmt.Errorf("network.static.gateway: invalid IP %q", c.Network.Static.Gateway)
		}
		for _, s := range c.Network.Static.DNS {
			if net.ParseIP(s) == nil {
				return fmt.Errorf("network.static.dns: invalid IP %q", s)
			}
		}
	}
	switch c.Cluster.Mode {
	case "none", "init", "join":
	default:
		return fmt.Errorf("cluster.mode: must be none, init or join, got %q", c.Cluster.Mode)
	}
	if c.Cluster.Mode == "join" && c.Cluster.JoinAddress == "" {
		return fmt.Errorf("cluster.join_address: required when mode is join")
	}
	if c.Timezone == "" {
		c.Timezone = "UTC"
	}
	return nil
}

// ResolvedLayout resolves the "auto" layout given the number of disks.
func (c *Config) ResolvedLayout(nDisks int) (DiskLayout, error) {
	switch c.Disks.Layout {
	case LayoutAuto:
		switch {
		case nDisks == 1:
			return LayoutSingle, nil
		case nDisks == 2:
			return LayoutMirror, nil
		case nDisks >= 3:
			return LayoutRaidz1, nil
		default:
			return "", fmt.Errorf("no disks selected")
		}
	case LayoutSingle:
		if nDisks != 1 {
			return "", fmt.Errorf("layout single requires exactly 1 disk, got %d", nDisks)
		}
		return LayoutSingle, nil
	case LayoutMirror:
		if nDisks < 2 {
			return "", fmt.Errorf("layout mirror requires at least 2 disks, got %d", nDisks)
		}
		return LayoutMirror, nil
	case LayoutRaidz1:
		if nDisks < 3 {
			return "", fmt.Errorf("layout raidz1 requires at least 3 disks, got %d", nDisks)
		}
		return LayoutRaidz1, nil
	}
	return "", fmt.Errorf("unknown layout %q", c.Disks.Layout)
}

// WriteYAML serializes the config (used to store it under /persist).
func (c *Config) WriteYAML(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal install config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// String returns a human-readable summary of the config.
func (c *Config) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "version: %d\n", c.Version)
	if c.Hostname != "" {
		fmt.Fprintf(&b, "hostname: %s\n", c.Hostname)
	}
	fmt.Fprintf(&b, "disks:\n  layout: %s\n", c.Disks.Layout)
	for _, d := range c.Disks.Devices {
		fmt.Fprintf(&b, "  device: %s\n", d)
	}
	fmt.Fprintf(&b, "network:\n  mode: %s\n", c.Network.Mode)
	if c.Network.Mode == "static" {
		fmt.Fprintf(&b, "  address: %s\n", c.Network.Static.Address)
		fmt.Fprintf(&b, "  gateway: %s\n", c.Network.Static.Gateway)
	}
	fmt.Fprintf(&b, "cluster:\n  mode: %s\n", c.Cluster.Mode)
	fmt.Fprintf(&b, "timezone: %s\n", c.Timezone)
	return b.String()
}
