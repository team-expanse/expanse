package systemd

import (
	"os"
	"testing"

	"github.com/expanse/expanse/internal/blocks/vm/vmstate"
)

func keepMarker(m *Manager, s Spec) string {
	return vmstate.KeepPath(m.KeepDir, Instance(s.Namespace, s.Name, s.Index))
}

func placedVM(cpu string) Spec {
	s := vmSpec(cpu)
	s.Placement = "a1b2"
	return s
}

func TestAnUpdateRequestRestartsTheVMKeepingItsGuest(t *testing.T) {
	m, api := runningVM(t, placedVM("1"), true)
	m.KeepDir = t.TempDir()
	next := placedVM("2") // a resize riding along resumes, then resizes
	next.Refresh = "2026-10-29T10:00:00Z"
	converge(t, m, load(t, m, next))
	if api.reloads != 0 || api.starts != 2 {
		t.Fatalf("reloads=%d starts=%d, want a restart", api.reloads, api.starts)
	}
	if _, err := os.Stat(keepMarker(m, next)); err != nil {
		t.Errorf("restarted without asking the VM to keep its guest: %v", err)
	}
}

func TestAnyOtherRestartDropsAStaleKeepRequest(t *testing.T) {
	m, api := runningVM(t, placedVM("1"), true)
	m.KeepDir = t.TempDir()
	if err := os.WriteFile(keepMarker(m, placedVM("1")), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	next := placedVM("1")
	next.Args[1] = `{"machine":"q35"}`
	next.Refresh = "2026-10-29T10:00:00Z"
	converge(t, m, load(t, m, next))
	if api.starts != 2 {
		t.Fatalf("starts=%d, want a restart", api.starts)
	}
	if _, err := os.Stat(keepMarker(m, next)); !os.IsNotExist(err) {
		t.Errorf("a cold restart kept the guest's keep request: %v", err)
	}
}

func TestANewPlacementIDReachesARunningVMByReload(t *testing.T) {
	m, api := runningVM(t, vmSpec("1"), true)
	next := vmSpec("1")
	next.Placement = "a1b2"
	converge(t, m, load(t, m, next))
	if api.reloads != 1 || api.starts != 1 {
		t.Fatalf("reloads=%d starts=%d, want one reload and no restart", api.reloads, api.starts)
	}
}

func TestAnUpdateRequestWithoutAPlacementIDRestartsColdly(t *testing.T) {
	m, api := runningVM(t, vmSpec("1"), true)
	m.KeepDir = t.TempDir()
	next := vmSpec("1")
	next.Refresh = "2026-10-29T10:00:00Z"
	converge(t, m, load(t, m, next))
	if _, err := os.Stat(keepMarker(m, next)); api.starts != 2 || !os.IsNotExist(err) {
		t.Errorf("starts=%d marker err %v, want a plain restart", api.starts, err)
	}
}
