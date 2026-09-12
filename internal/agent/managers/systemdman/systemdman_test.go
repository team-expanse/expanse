package systemdman

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/expanse/expanse/internal/reconcile"
)

// fakeConn is a mocked systemd D-Bus API for unit tests (the plan's
// "mocked dbus: enable/start/restart paths").
type fakeConn struct {
	mu      sync.Mutex
	units   map[string]*fakeUnit // by unit name
	actions []string             // recorded enable/disable/start/stop/restart
}

type fakeUnit struct {
	load, active, sub string
	enabled           bool
}

func newFakeConn(units map[string]*fakeUnit) *fakeConn {
	return &fakeConn{units: units}
}

func (c *fakeConn) record(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.actions = append(c.actions, fmt.Sprintf(format, args...))
}

func (c *fakeConn) UnitState(ctx context.Context, unit string) (string, string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	u, ok := c.units[unit]
	if !ok {
		return "not-found", "inactive", "dead", nil
	}
	return u.load, u.active, u.sub, nil
}

func (c *fakeConn) UnitEnabled(ctx context.Context, unit string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	u, ok := c.units[unit]
	if !ok {
		return false, nil
	}
	return u.enabled, nil
}

func (c *fakeConn) Enable(ctx context.Context, unit string) error {
	c.record("enable %s", unit)
	c.mu.Lock()
	defer c.mu.Unlock()
	if u, ok := c.units[unit]; ok {
		u.enabled = true
	}
	return nil
}

func (c *fakeConn) Disable(ctx context.Context, unit string) error {
	c.record("disable %s", unit)
	c.mu.Lock()
	defer c.mu.Unlock()
	if u, ok := c.units[unit]; ok {
		u.enabled = false
	}
	return nil
}

func (c *fakeConn) Start(ctx context.Context, unit string) error {
	c.record("start %s", unit)
	c.mu.Lock()
	defer c.mu.Unlock()
	if u, ok := c.units[unit]; ok {
		u.active = "active"
		u.sub = "running"
	}
	return nil
}

func (c *fakeConn) Stop(ctx context.Context, unit string) error {
	c.record("stop %s", unit)
	c.mu.Lock()
	defer c.mu.Unlock()
	if u, ok := c.units[unit]; ok {
		u.active = "inactive"
		u.sub = "dead"
	}
	return nil
}

func (c *fakeConn) Restart(ctx context.Context, unit string) error {
	c.record("restart %s", unit)
	return c.Start(ctx, unit)
}

func mustLoad(t *testing.T, spec string) reconcile.Resource {
	t.Helper()
	m := NewWithConn(nil)
	r, err := m.Load("systemd-unit:foo.service", []byte(spec))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return r
}

func runOnce(t *testing.T, conn *fakeConn, r reconcile.Resource) []reconcile.Action {
	t.Helper()
	m := NewWithConn(conn)
	o, err := m.Observe(context.Background(), r)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	acts, err := m.Plan(context.Background(), r, o)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, a := range acts {
		if err := m.Apply(context.Background(), a); err != nil {
			t.Fatalf("Apply %s: %v", a.Description, err)
		}
	}
	// Re-observe: must now be in sync.
	o, err = m.Observe(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !o.InSync {
		t.Errorf("after apply still out of sync: %+v", o)
	}
	return acts
}

func TestEnableStartPath(t *testing.T) {
	conn := newFakeConn(map[string]*fakeUnit{
		"foo.service": {load: "loaded", active: "inactive", sub: "dead", enabled: false},
	})
	r := mustLoad(t, "name: foo.service\nenabled: true\nstate: running\n")
	acts := runOnce(t, conn, r)
	if len(acts) != 2 {
		t.Fatalf("expected enable+start actions, got %v", acts)
	}
	wantActions := []string{"enable foo.service", "start foo.service"}
	for i, w := range wantActions {
		if conn.actions[i] != w {
			t.Errorf("action[%d] = %q, want %q", i, conn.actions[i], w)
		}
	}
}

func TestDisableStopPath(t *testing.T) {
	conn := newFakeConn(map[string]*fakeUnit{
		"foo.service": {load: "loaded", active: "active", sub: "running", enabled: true},
	})
	r := mustLoad(t, "name: foo.service\nenabled: false\nstate: stopped\n")
	acts := runOnce(t, conn, r)
	if len(acts) != 2 {
		t.Fatalf("expected disable+stop actions, got %v", acts)
	}
	if conn.actions[0] != "disable foo.service" || conn.actions[1] != "stop foo.service" {
		t.Errorf("actions = %v, want [disable stop]", conn.actions)
	}
}

func TestRestartFailedUnit(t *testing.T) {
	conn := newFakeConn(map[string]*fakeUnit{
		"foo.service": {load: "loaded", active: "active", sub: "failed", enabled: true},
	})
	r := mustLoad(t, "name: foo.service\n")
	m := NewWithConn(conn)
	o, err := m.Observe(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	// A failed-but-active unit is not in sync.
	if o.InSync {
		t.Fatal("failed sub-state should count as out of sync")
	}
	acts, err := m.Plan(context.Background(), r, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 || acts[0].Kind != "restart" {
		t.Fatalf("expected restart action, got %v", acts)
	}
	if err := m.Apply(context.Background(), acts[0]); err != nil {
		t.Fatal(err)
	}
}

func TestInSyncNoActions(t *testing.T) {
	conn := newFakeConn(map[string]*fakeUnit{
		"foo.service": {load: "loaded", active: "active", sub: "running", enabled: true},
	})
	r := mustLoad(t, "name: foo.service\nenabled: true\nstate: running\n")
	m := NewWithConn(conn)
	o, err := m.Observe(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !o.InSync {
		t.Fatalf("healthy unit should be in sync: %+v", o)
	}
	acts, err := m.Plan(context.Background(), r, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 0 {
		t.Errorf("in-sync unit planned actions: %v", acts)
	}
}

func TestMissingUnitFile(t *testing.T) {
	conn := newFakeConn(map[string]*fakeUnit{})
	r := mustLoad(t, "name: ghost.service\n")
	m := NewWithConn(conn)
	o, err := m.Observe(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if o.Exists || o.Health != reconcile.HealthUnhealthy {
		t.Errorf("missing unit: %+v, want exists=false unhealthy", o)
	}
}

func TestBadSpecs(t *testing.T) {
	m := NewWithConn(nil)
	cases := []string{
		"enabled: true\n",                    // missing name
		"name: foo\n",                        // missing suffix
		"name: foo.service\nstate: paused\n", // bad state
	}
	for i, spec := range cases {
		if _, err := m.Load("systemd-unit:x", []byte(spec)); err == nil {
			t.Errorf("case %d: expected error for spec %q", i, spec)
		}
	}
}
