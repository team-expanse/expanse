package config

import (
	"fmt"
	"net"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Config holds the application configuration.
type Config struct {
	NodeName  string         `yaml:"node_name"`
	DataDir   string         `yaml:"data_dir"`
	LogLevel  string         `yaml:"log_level"`
	LogFormat string         `yaml:"log_format"`
	Listen    ListenConfig   `yaml:"listen"`
}

// ListenConfig holds listen address configuration.
type ListenConfig struct {
	Agent string `yaml:"agent"` // default 0.0.0.0:7443
	UI    string `yaml:"ui"`    // default 0.0.0.0:8443
}

// defaults returns the built-in defaults.
func defaults() *Config {
	return &Config{
		DataDir:   "/persist/expanse",
		LogLevel:  "info",
		LogFormat: "text",
		Listen: ListenConfig{
			Agent: "0.0.0.0:7443",
			UI:    "0.0.0.0:8443",
		},
	}
}

// Load reads configuration from path (may be empty for defaults).
func Load(path string) (*Config, error) {
	c := defaults()
	if path == "" {
		return c, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return c, nil
}

// Validate checks configuration invariants.
func (c *Config) Validate() error {
	if c.DataDir == "" {
		return fmt.Errorf("data_dir must not be empty")
	}
	if _, err := os.Stat(c.DataDir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("data_dir %s: %w", c.DataDir, err)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("unknown log_level: %q", c.LogLevel)
	}
	if c.Listen.Agent != "" {
		if err := validAddr(c.Listen.Agent); err != nil {
			return fmt.Errorf("invalid agent listen address %q: %w", c.Listen.Agent, err)
		}
	}
	if c.Listen.UI != "" {
		if err := validAddr(c.Listen.UI); err != nil {
			return fmt.Errorf("invalid ui listen address %q: %w", c.Listen.UI, err)
		}
	}
	return nil
}

// validAddr checks that addr is host:port with a numeric port.
func validAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("port %q is not numeric", port)
	}
	return nil
}