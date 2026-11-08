package upgrade

import (
	"os"
	"path/filepath"
	"testing"
)

// bootFiles makes a system closure whose kernel, initrd and kernel-modules link to the given targets.
func bootFiles(t *testing.T, store, kernel, initrd, modules string) string {
	t.Helper()
	sys := t.TempDir()
	for name, target := range map[string]string{"kernel": kernel, "initrd": initrd, "kernel-modules": modules} {
		p := filepath.Join(store, target)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(p, filepath.Join(sys, name)); err != nil {
			t.Fatal(err)
		}
	}
	return sys
}

func TestRebootNeeded(t *testing.T) {
	store := t.TempDir()
	booted := bootFiles(t, store, "k1", "i1", "m1")
	for _, tc := range []struct {
		name   string
		system string
		want   bool
	}{
		{"same boot files", bootFiles(t, store, "k1", "i1", "m1"), false},
		{"new kernel", bootFiles(t, store, "k2", "i1", "m1"), true},
		{"new initrd", bootFiles(t, store, "k1", "i2", "m1"), true},
		{"new kernel modules", bootFiles(t, store, "k1", "i1", "m2"), true},
		{"system without boot files", t.TempDir(), true},
	} {
		if got := RebootNeeded(booted, tc.system); got != tc.want {
			t.Errorf("%s: RebootNeeded = %v, want %v", tc.name, got, tc.want)
		}
	}
	if RebootNeeded("", booted) {
		t.Error("no booted system (not NixOS) must not ask for a reboot")
	}
}
