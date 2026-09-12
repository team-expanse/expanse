// Package systemdman converges systemd units via the D-Bus API
// (github.com/coreos/go-systemd/v22/dbus) — structured state and job
// completion signals instead of parsing `systemctl` output (D2.5).
package systemdman

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/reconcile"
	"gopkg.in/yaml.v3"
)

// Spec is the desired state of a systemd unit resource.
type Spec struct {
	Name    string `yaml:"name"`    // unit name including suffix, e.g. "foo.service"
	Enabled *bool  `yaml:"enabled"` // wanted by multi-user.target (default true)
	State   string `yaml:"state"`   // "running" | "stopped" (default running)
}

// Resource is a systemd-unit resource.
type Resource struct {
	id   string
	spec Spec
}

// ID implements reconcile.Resource.
func (r *Resource) ID() string { return r.id }

// Type implements reconcile.Resource.
func (r *Resource) Type() string { return "systemd-unit" }

// Dependencies implements reconcile.Resource.
func (r *Resource) Dependencies() []string { return nil }

// systemdAPI is the subset of the D-Bus API the manager uses; fakeConn
// implements it for tests.
type systemdAPI interface {
	// UnitState returns the unit's load/active/sub state triple.
	UnitState(ctx context.Context, unit string) (loadState, activeState, subState string, err error)
	// UnitEnabled reports whether the unit is enabled.
	UnitEnabled(ctx context.Context, unit string) (bool, error)
	// Enable/Disable create/remove the symlink in the default target.
	Enable(ctx context.Context, unit string) error
	Disable(ctx context.Context, unit string) error
	// Start/Stop submit a job and wait for completion.
	Start(ctx context.Context, unit string) error
	Stop(ctx context.Context, unit string) error
	// Restart restarts a running unit (no-op if inactive; callers decide).
	Restart(ctx context.Context, unit string) error
}

// dbusConn adapts the real systemd D-Bus connection to systemdAPI.
type dbusConn struct{ conn *dbus.Conn }

func (c *dbusConn) UnitState(ctx context.Context, unit string) (string, string, string, error) {
	units, err := c.conn.ListUnitsByNamesContext(ctx, []string{unit})
	if err != nil {
		return "", "", "", err
	}
	if len(units) == 0 {
		return "not-found", "inactive", "dead", nil
	}
	u := units[0]
	return u.LoadState, u.ActiveState, u.SubState, nil
}

func (c *dbusConn) UnitEnabled(ctx context.Context, unit string) (bool, error) {
	files, err := c.conn.ListUnitFilesContext(ctx)
	if err != nil {
		return false, err
	}
	for _, f := range files {
		if strings.HasSuffix(f.Path, "/"+unit) {
			return f.Type == "enabled", nil
		}
	}
	return false, nil
}

func (c *dbusConn) Enable(ctx context.Context, unit string) error {
	_, _, err := c.conn.EnableUnitFilesContext(ctx, []string{unit}, false, true)
	return err
}

func (c *dbusConn) Disable(ctx context.Context, unit string) error {
	_, err := c.conn.DisableUnitFilesContext(ctx, []string{unit}, false)
	return err
}

func (c *dbusConn) Start(ctx context.Context, unit string) error {
	return c.submitJob(ctx, unit, "active", func(ch chan<- string) error {
		_, err := c.conn.StartUnitContext(ctx, unit, "replace", ch)
		return err
	})
}

func (c *dbusConn) Stop(ctx context.Context, unit string) error {
	return c.submitJob(ctx, unit, "inactive", func(ch chan<- string) error {
		_, err := c.conn.StopUnitContext(ctx, unit, "replace", ch)
		return err
	})
}

func (c *dbusConn) Restart(ctx context.Context, unit string) error {
	return c.submitJob(ctx, unit, "active", func(ch chan<- string) error {
		_, err := c.conn.RestartUnitContext(ctx, unit, "replace", ch)
		return err
	})
}

// submitJob runs a systemd job and waits for its completion signal (the
// job result channel), then verifies the unit reached the desired state.
func (c *dbusConn) submitJob(ctx context.Context, unit, wantActive string, submit func(ch chan<- string) error) error {
	ch := make(chan string, 1)
	if err := submit(ch); err != nil {
		return err
	}
	select {
	case result := <-ch:
		if result != "done" {
			return fmt.Errorf("job for %s %s", unit, result)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	// Verify final state (start of an already-started unit is a no-op job
	// that reports "done"; the unit state is the ground truth).
	for i := 0; i < 20; i++ {
		_, active, _, err := c.UnitState(ctx, unit)
		if err != nil {
			return err
		}
		if active == wantActive {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("unit %s did not reach %s in time", unit, wantActive)
}

// Manager handles "systemd-unit" resources.
type Manager struct {
	conn systemdAPI
}

// New creates a Manager over the real system D-Bus. Fails if systemd is
// not present (e.g. in unit tests).
func New() (*Manager, error) {
	conn, err := dbus.NewSystemdConnectionContext(context.Background())
	if err != nil {
		return nil, errors.Wrap(err, errors.KindUnavailable, "systemdman.New", "connect to systemd dbus")
	}
	return &Manager{conn: &dbusConn{conn: conn}}, nil
}

// NewWithConn creates a Manager over an injected API (tests).
func NewWithConn(conn systemdAPI) *Manager { return &Manager{conn: conn} }

// Type implements reconcile.Manager.
func (m *Manager) Type() string { return "systemd-unit" }

// Load implements reconcile.Manager.
func (m *Manager) Load(id string, spec []byte) (reconcile.Resource, error) {
	var s Spec
	if err := yaml.Unmarshal(spec, &s); err != nil {
		return nil, errors.Wrap(err, errors.KindInvalid, "systemdman.Load", "bad spec")
	}
	if s.Name == "" {
		return nil, errors.New(errors.KindInvalid, "systemdman.Load", "spec missing unit name")
	}
	if !strings.Contains(s.Name, ".") {
		return nil, errors.New(errors.KindInvalid, "systemdman.Load", "unit name must include suffix, e.g. .service")
	}
	if s.State == "" {
		s.State = "running"
	}
	if s.State != "running" && s.State != "stopped" {
		return nil, errors.New(errors.KindInvalid, "systemdman.Load", "state must be running|stopped")
	}
	if s.Enabled == nil {
		t := true
		s.Enabled = &t
	}
	return &Resource{id: id, spec: s}, nil
}

// Observe implements reconcile.Manager.
func (m *Manager) Observe(ctx context.Context, r reconcile.Resource) (reconcile.Observed, error) {
	res, ok := r.(*Resource)
	if !ok {
		return reconcile.Observed{}, errors.New(errors.KindInvalid, "systemdman.Observe", "wrong resource type")
	}
	unit := res.spec.Name
	loadState, activeState, subState, err := m.conn.UnitState(ctx, unit)
	if err != nil {
		return reconcile.Observed{}, errors.Wrap(err, errors.KindInternal, "systemdman.Observe", "query unit state")
	}
	if loadState == "not-found" {
		return reconcile.Observed{
			Exists: false, InSync: false, Health: reconcile.HealthUnhealthy,
			Details: map[string]string{"unit": unit, "problem": "unit file not found"},
		}, nil
	}
	enabled, err := m.conn.UnitEnabled(ctx, unit)
	if err != nil {
		return reconcile.Observed{}, errors.Wrap(err, errors.KindInternal, "systemdman.Observe", "query enabled")
	}
	wantActive := res.spec.State == "running"
	inSync := (activeState == "active") == wantActive &&
		enabled == *res.spec.Enabled && subState != "failed"
	h := reconcile.HealthDegraded
	if inSync {
		h = reconcile.HealthHealthy
	}
	return reconcile.Observed{
		Exists: true,
		InSync: inSync,
		Health: h,
		Details: map[string]string{
			"unit": unit, "load": loadState, "active": activeState,
			"sub": subState, "enabled": fmt.Sprintf("%t", enabled),
		},
	}, nil
}

// Plan implements reconcile.Manager.
func (m *Manager) Plan(ctx context.Context, r reconcile.Resource, o reconcile.Observed) ([]reconcile.Action, error) {
	res, ok := r.(*Resource)
	if !ok {
		return nil, errors.New(errors.KindInvalid, "systemdman.Plan", "wrong resource type")
	}
	unit := res.spec.Name
	wantActive := res.spec.State == "running"
	var actions []reconcile.Action
	if !o.Exists {
		return nil, errors.New(errors.KindInvalid, "systemdman.Plan",
			"unit file does not exist; install the unit file first (nix-config or file resource)")
	}
	if o.Details["enabled"] != fmt.Sprintf("%t", *res.spec.Enabled) {
		if *res.spec.Enabled {
			actions = append(actions, reconcile.Action{
				ResourceID:  res.id,
				Kind:        "update",
				Description: "systemctl enable " + unit,
				Fn:          func(ctx context.Context) error { return m.conn.Enable(ctx, unit) },
			})
		} else {
			actions = append(actions, reconcile.Action{
				ResourceID:  res.id,
				Kind:        "update",
				Description: "systemctl disable " + unit,
				Fn:          func(ctx context.Context) error { return m.conn.Disable(ctx, unit) },
			})
		}
	}
	isActive := o.Details["active"] == "active"
	if isActive != wantActive {
		if wantActive {
			actions = append(actions, reconcile.Action{
				ResourceID:  res.id,
				Kind:        "create",
				Description: "systemctl start " + unit,
				Fn:          func(ctx context.Context) error { return m.conn.Start(ctx, unit) },
			})
		} else {
			actions = append(actions, reconcile.Action{
				ResourceID:  res.id,
				Kind:        "update",
				Description: "systemctl stop " + unit,
				Fn:          func(ctx context.Context) error { return m.conn.Stop(ctx, unit) },
			})
		}
	} else if wantActive && o.Details["sub"] == "failed" {
		actions = append(actions, reconcile.Action{
			ResourceID:  res.id,
			Kind:        "restart",
			Description: "systemctl restart " + unit + " (failed)",
			Fn:          func(ctx context.Context) error { return m.conn.Restart(ctx, unit) },
		})
	}
	return actions, nil
}

// Apply implements reconcile.Manager.
func (m *Manager) Apply(ctx context.Context, a reconcile.Action) error {
	if a.Fn == nil {
		return errors.New(errors.KindInternal, "systemdman.Apply", "action has no function")
	}
	return a.Fn(ctx)
}

// Delete implements reconcile.Deleter: disable and stop the unit.
func (m *Manager) Delete(ctx context.Context, r reconcile.Resource) error {
	res, ok := r.(*Resource)
	if !ok {
		return errors.New(errors.KindInvalid, "systemdman.Delete", "wrong resource type")
	}
	if err := m.conn.Disable(ctx, res.spec.Name); err != nil {
		return err
	}
	return m.conn.Stop(ctx, res.spec.Name)
}
