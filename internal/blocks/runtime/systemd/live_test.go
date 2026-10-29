package systemd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/expanse/expanse/internal/blocks/livespec"
)

func vmSpec(cpu string) Spec {
	s := sampleSpec()
	s.Type, s.RunAsRoot = "vm/instance", true
	s.Args = []string{"--config", `{"machine":"pc"}`, "--cpu", cpu, "--mem", "1Gi"}
	return s
}

// runningVM converges spec on a fresh manager and, when live, marks its runtime as serving reloads.
func runningVM(t *testing.T, spec Spec, live bool) (*Manager, *fakeUnitAPI) {
	t.Helper()
	api := &fakeUnitAPI{states: map[string][3]string{}, status: map[string]string{}}
	m, _ := converged(t, api, spec)
	m.LiveDir = t.TempDir()
	if live {
		marker := livespec.MarkerPath(m.LiveDir, Instance(spec.Namespace, spec.Name, spec.Index))
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return m, api
}

func TestAResizeReloadsTheRunningVM(t *testing.T) {
	m, api := runningVM(t, vmSpec("1"), true)
	res := load(t, m, vmSpec("2"))
	converge(t, m, res)
	if api.reloads != 1 || api.starts != 1 {
		t.Fatalf("reloads=%d starts=%d, want one reload and no restart", api.reloads, api.starts)
	}
	if !m.specOnDiskMatches(vmSpec("2")) {
		t.Error("the reload ran before the new spec was written")
	}
	o, err := m.Observe(context.Background(), res)
	if err != nil {
		t.Fatal(err)
	}
	if !o.InSync {
		t.Error("not in sync after the reload")
	}
}

func TestAChangeBeyondTheLiveFieldsRestarts(t *testing.T) {
	m, api := runningVM(t, vmSpec("1"), true)
	next := vmSpec("2")
	next.Args[1] = `{"machine":"q35"}`
	converge(t, m, load(t, m, next))
	if api.reloads != 0 || api.starts != 2 {
		t.Fatalf("reloads=%d starts=%d, want a restart", api.reloads, api.starts)
	}
	marker := livespec.MarkerPath(m.LiveDir, Instance(next.Namespace, next.Name, next.Index))
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("the restart kept the old run's reload marker: %v", err)
	}
}

func TestARuntimeWithoutReloadsIsRestarted(t *testing.T) {
	m, api := runningVM(t, vmSpec("1"), false)
	converge(t, m, load(t, m, vmSpec("2")))
	if api.reloads != 0 || api.starts != 2 {
		t.Fatalf("reloads=%d starts=%d, want a restart", api.reloads, api.starts)
	}
}

func TestAStoppedUnitIsStartedRatherThanReloaded(t *testing.T) {
	m, api := runningVM(t, vmSpec("1"), true)
	api.states[UnitNameForSpec(vmSpec("1"))] = [3]string{"loaded", "failed", "failed"}
	converge(t, m, load(t, m, vmSpec("2")))
	if api.reloads != 0 || api.starts != 2 {
		t.Fatalf("reloads=%d starts=%d, want a start", api.reloads, api.starts)
	}
}

func TestDeletingAReplicaDropsItsReloadMarker(t *testing.T) {
	m, _ := runningVM(t, vmSpec("1"), true)
	m.StateDir = t.TempDir()
	if err := m.Delete(context.Background(), load(t, m, vmSpec("1"))); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(m.LiveDir); len(ents) != 0 {
		t.Errorf("left %s", filepath.Join(m.LiveDir, ents[0].Name()))
	}
}
