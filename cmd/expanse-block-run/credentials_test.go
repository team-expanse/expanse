package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func configArg(t *testing.T, args []string) map[string]any {
	t.Helper()
	i := slices.Index(args, "--config")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("no --config in %v", args)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(args[i+1]), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCredentialsGoBackIntoTheConfig(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "config_superuserPassword"), []byte("pg-pass"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "other"), []byte("not ours"), 0o600)
	args, err := withCredentials([]string{"--config", `{"port":5432}`, "--mount", "data=/x"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := configArg(t, args)
	if cfg["superuserPassword"] != "pg-pass" || cfg["port"] != float64(5432) || len(cfg) != 2 {
		t.Errorf("config = %v", cfg)
	}
	if !slices.Equal(args[2:], []string{"--mount", "data=/x"}) {
		t.Errorf("other args changed: %v", args)
	}
}

func TestCredentialsWithoutAConfigArgAddOne(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "config_password"), []byte("p"), 0o600)
	args, err := withCredentials([]string{"--cpu", "1"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if configArg(t, args)["password"] != "p" {
		t.Errorf("args = %v", args)
	}
}

func TestNoCredentialsLeaveTheArgsAlone(t *testing.T) {
	in := []string{"--config", `{"a":1}`}
	for _, dir := range []string{"", t.TempDir()} {
		if out, err := withCredentials(in, dir); err != nil || !slices.Equal(out, in) {
			t.Errorf("dir %q: %v, %v", dir, out, err)
		}
	}
}
