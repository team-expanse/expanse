package install

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunDryRunTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "install.yaml")
	if err := os.WriteFile(cfgPath, []byte(validYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := Run(Options{
		ConfigPath: cfgPath,
		DryRun:     true,
		Logger:     func(f string, a ...any) { out.WriteString(fmt.Sprintf(f, a...) + "\n") },
	})
	if err != nil {
		t.Fatalf("Run --dry-run: %v", err)
	}

	text := out.String()
	for _, stage := range []string{"preflight", "detect", "confirm", "partition", "snapshot", "config", "install", "identity", "verify"} {
		if !strings.Contains(text, stage) {
			t.Errorf("dry-run output missing stage %q", stage)
		}
	}
	if !strings.Contains(text, "zfs snapshot rpool/root@blank") {
		t.Error("dry-run output missing the blank snapshot command")
	}
	if !strings.Contains(text, "disko") {
		t.Error("dry-run output missing the disko command")
	}

	// Nothing may have been written to the target system.
	if _, err := os.Stat("/mnt"); err == nil {
		t.Log("/mnt exists on this machine; skipping mountpoint check")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("dry-run wrote files into config dir: %v", entries)
	}
}
