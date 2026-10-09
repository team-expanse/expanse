package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestUptimeKumaSetupKeepsDataOnTheVolume(t *testing.T) {
	s := uptimeKumaSetupFrom("/vol", "/tmp/kuma", map[string]any{})
	if s.DataDir != "/vol/uptime-kuma" || s.Port != "3001" || s.Preload != "/tmp/kuma/sync-full.cjs" {
		t.Errorf("setup = %+v", s)
	}
	if s := uptimeKumaSetupFrom("/vol", "/tmp/kuma", map[string]any{"port": float64(3100)}); s.Port != "3100" {
		t.Errorf("port = %s, want 3100", s.Port)
	}
}

func TestUptimeKumaEnv(t *testing.T) {
	s := uptimeKumaSetupFrom("/vol", "/tmp/kuma", map[string]any{})
	want := []string{
		"DATA_DIR=/vol/uptime-kuma",
		"HOME=/tmp/kuma",
		"NODE_OPTIONS=--require=/tmp/kuma/sync-full.cjs",
		// Skips the first-run page that asks which database to use.
		"UPTIME_KUMA_DB_TYPE=sqlite",
		"UPTIME_KUMA_PORT=3001",
	}
	if got := uptimeKumaEnv(s); !slices.Equal(got, want) {
		t.Errorf("env = %v, want %v", got, want)
	}
}

func TestUptimeKumaPreloadForcesSynchronousFull(t *testing.T) {
	// Uptime Kuma hard-codes synchronous = NORMAL; the preload rewrites that pragma for its SQLite driver.
	for _, want := range []string{`"@louislam/sqlite3"`, `PRAGMA synchronous = FULL`} {
		if !strings.Contains(uptimeKumaPreload, want) {
			t.Errorf("preload lacks %s", want)
		}
	}
}

func TestWriteUptimeKumaPreload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync-full.cjs")
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeUptimeKumaPreload(path); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != uptimeKumaPreload {
		t.Errorf("preload = %q", got)
	}
}
