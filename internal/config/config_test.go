package config

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.DataDir != "/persist/expanse" {
		t.Errorf("expected default data_dir %q, got %q", "/persist/expanse", c.DataDir)
	}
	if c.LogLevel != "info" {
		t.Errorf("expected default log_level %q, got %q", "info", c.LogLevel)
	}
}

func TestLoadNonexistentFile(t *testing.T) {
	c, err := Load("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.DataDir == "" {
		t.Error("should have defaults when file not found")
	}
}

func TestValidateEmptyDataDir(t *testing.T) {
	c := &Config{DataDir: "", LogLevel: "info"}
	err := c.Validate()
	if err == nil {
		t.Error("expected error for empty data_dir")
	}
}

func TestValidateInvalidLogLevel(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	os.WriteFile(cfgPath, []byte("log_level: bogus\n"), 0o644)
	c, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	err = c.Validate()
	if err == nil {
		t.Error("expected error for invalid log level")
	}
}

func TestValidateValid(t *testing.T) {
	tmpDir := t.TempDir()
	c := &Config{DataDir: tmpDir, LogLevel: "info", LogFormat: "text"}
	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateInvalidListenAddress(t *testing.T) {
	tmpDir := t.TempDir()
	c := &Config{DataDir: tmpDir, LogLevel: "info", Listen: ListenConfig{Agent: ":invalid"}}
	err := c.Validate()
	if err == nil {
		t.Error("expected error for invalid listen address")
	}
}

func TestListenSplitHostPort(t *testing.T) {
	host, port, err := net.SplitHostPort("0.0.0.0:7443")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "0.0.0.0" || port != "7443" {
		t.Errorf("expected 0.0.0.0:7443, got %s:%s", host, port)
	}
}
