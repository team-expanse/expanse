package drbd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProc(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "drbd")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// After enabling storage with a switch, modprobe loads the kernel's in-tree DRBD 8.4.
func TestCheckModuleRefusesDRBD8(t *testing.T) {
	err := CheckModule(writeProc(t, "version: 8.4.11 (api:1/proto:86-101)\nsrcversion: 1A2B\n"))
	if err == nil || !strings.Contains(err.Error(), "8.4.11") || !strings.Contains(err.Error(), "reboot") {
		t.Fatalf("CheckModule = %v, want a DRBD 8.4.11 error that says to reboot", err)
	}
}

func TestCheckModuleAcceptsDRBD9(t *testing.T) {
	if err := CheckModule(writeProc(t, "version: 9.2.12 (api:2/proto:86-122)\nGIT-hash: abc\n")); err != nil {
		t.Fatalf("CheckModule = %v", err)
	}
}

// Not loaded yet: drbdadm loads it, from the booted system's DRBD 9.
func TestCheckModuleAllowsUnloaded(t *testing.T) {
	if err := CheckModule(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("CheckModule = %v", err)
	}
}
