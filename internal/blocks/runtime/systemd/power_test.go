package systemd

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/expanse/expanse/internal/blocks/vm/vmstate"
	"github.com/expanse/expanse/internal/reconcile"
)

func suspendedSpec(token string) Spec {
	s := sampleSpec()
	s.Type, s.RunAsRoot = "vm/instance", true
	s.Stopped, s.StateToken = true, token
	return s
}

// load re-reads spec into a resource on m, as the reconciler does on each tick.
func load(t *testing.T, m *Manager, spec Spec) reconcile.Resource {
	t.Helper()
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	res, err := m.Load("block-replica:default/x/0", b)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func converge(t *testing.T, m *Manager, res reconcile.Resource) {
	t.Helper()
	ctx := context.Background()
	o, err := m.Observe(ctx, res)
	if err != nil {
		t.Fatal(err)
	}
	acts, err := m.Plan(ctx, res, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range acts {
		if err := m.Apply(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAStoppedSpecStopsTheRunningUnit(t *testing.T) {
	running := suspendedSpec("r5")
	running.Stopped, running.StateToken = false, ""
	api := &fakeUnitAPI{states: map[string][3]string{}, status: map[string]string{}}
	m, _ := converged(t, api, running)
	m.StateDir = t.TempDir()
	unit := UnitNameForSpec(running)

	res := load(t, m, suspendedSpec("r5"))
	converge(t, m, res)
	if got := api.states[unit][1]; got != "inactive" {
		t.Fatalf("unit is %q, want inactive", got)
	}
	o, err := m.Observe(context.Background(), res)
	if err != nil {
		t.Fatal(err)
	}
	if !o.InSync || o.Health != reconcile.HealthHealthy || o.Ready != nil {
		t.Errorf("stopped: in_sync=%v health=%v ready=%v, want in sync, healthy, unreported", o.InSync, o.Health, o.Ready)
	}
	if o.Details["status"] != "stopped" {
		t.Errorf("status = %q, want stopped (nothing saved)", o.Details["status"])
	}
	starts := api.starts
	converge(t, m, res)
	if api.starts != starts {
		t.Error("a stopped replica was started again")
	}
}

func TestAStoppedReplicaReportsTheSaveMatchingItsToken(t *testing.T) {
	api := &fakeUnitAPI{states: map[string][3]string{}, status: map[string]string{}}
	m, res := converged(t, api, suspendedSpec("r5"))
	m.StateDir = t.TempDir()
	dir := vmstate.Dir(m.StateDir, Instance("default", sampleSpec().Name, sampleSpec().Index))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(vmstate.TempPath(dir), []byte("ram"), 0o600)
	if err := vmstate.Commit(dir, "r5"); err != nil {
		t.Fatal(err)
	}
	o, _ := m.Observe(context.Background(), res)
	if o.Details["status"] != "suspended r5" {
		t.Errorf("status = %q, want suspended r5", o.Details["status"])
	}
	o, _ = m.Observe(context.Background(), load(t, m, suspendedSpec("r6")))
	if o.Details["status"] != "stopped" {
		t.Errorf("another suspension's save: status = %q, want stopped", o.Details["status"])
	}
}

func TestDeletingAReplicaDropsItsSavedState(t *testing.T) {
	api := &fakeUnitAPI{states: map[string][3]string{}, status: map[string]string{}}
	m, res := converged(t, api, suspendedSpec("r5"))
	m.StateDir = t.TempDir()
	dir := vmstate.Dir(m.StateDir, Instance("default", sampleSpec().Name, sampleSpec().Index))
	_ = os.MkdirAll(dir, 0o700)
	if err := m.Delete(context.Background(), res); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("saved state survived the delete: %v", err)
	}
}
