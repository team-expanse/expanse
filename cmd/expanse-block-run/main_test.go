package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The exact file bytes the agent writes (double-escaped config JSON).
func TestSpecParseExact(t *testing.T) {
	dir := t.TempDir()
	raw := `{"namespace":"default","name":"web","index":0,"type":"util/echo","args":["--config","{\"body\":\"deploy-test\\n\",\"port\":18080}"]}`
	if err := os.WriteFile(filepath.Join(dir, "default-web-0.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Setenv("SPEC_DIR_OVERRIDE", dir)
	s, err := loadSpecAt(dir, "default-web-0")
	if err != nil {
		t.Fatal(err)
	}
	if s.Type != "util/echo" || len(s.Args) != 2 {
		t.Fatalf("spec: %+v", s)
	}
	var cfg struct {
		Port float64 `json:"port"`
		Body string  `json:"body"`
	}
	if err := json.Unmarshal([]byte(s.Args[1]), &cfg); err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.Body != "deploy-test\n" || cfg.Port != 18080 {
		t.Fatalf("cfg: %q %v", cfg.Body, cfg.Port)
	}
	_ = strings.TrimSpace("")
}
