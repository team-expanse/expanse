// Package nixman converges nix-config resources: build a flake attribute
// and switch the system to it, via the nix driver.
package nixman

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/expanse/expanse/internal/agent/nix"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/reconcile"
	"gopkg.in/yaml.v3"
)

// Spec is the desired state of a nix-config resource.
type Spec struct {
	// FlakeRef is the flake to build, e.g. "path:/etc/nixos".
	FlakeRef string `yaml:"flake"`
	// Attr is the attribute to build, e.g. "nixosConfigurations.expanse-node.config.system.build.toplevel".
	Attr string `yaml:"attr"`
	// SwitchMode: test | boot | switch (default switch).
	SwitchMode string `yaml:"switch_mode"`
}

// Resource is a nix-config resource.
type Resource struct {
	id   string
	spec Spec
}

// ID implements reconcile.Resource.
func (r *Resource) ID() string { return r.id }

// Type implements reconcile.Resource.
func (r *Resource) Type() string { return "nix-config" }

// Dependencies implements reconcile.Resource.
func (r *Resource) Dependencies() []string { return nil }

// Manager handles "nix-config" resources.
type Manager struct {
	driver nix.Driver
	// markerDir holds per-resource markers recording the system path the
	// agent last switched this resource to (observed vs desired cheaply).
	markerDir string
	logs      io.Writer // build log destination (event log / SSE channel)
}

// New creates the manager over a driver; markerDir defaults to
// /persist/expanse/nix-config.
func New(driver nix.Driver, logs io.Writer) *Manager {
	return &Manager{driver: driver, markerDir: "/persist/expanse/nix-config", logs: logs}
}

// NewWithMarkerDir creates the manager with an explicit marker dir (tests).
func NewWithMarkerDir(driver nix.Driver, markerDir string, logs io.Writer) *Manager {
	return &Manager{driver: driver, markerDir: markerDir, logs: logs}
}

// Type implements reconcile.Manager.
func (m *Manager) Type() string { return "nix-config" }

// Load implements reconcile.Manager.
func (m *Manager) Load(id string, spec []byte) (reconcile.Resource, error) {
	var s Spec
	if err := yaml.Unmarshal(spec, &s); err != nil {
		return nil, errors.Wrap(err, errors.KindInvalid, "nixman.Load", "bad spec")
	}
	if s.FlakeRef == "" {
		return nil, errors.New(errors.KindInvalid, "nixman.Load", "spec missing flake")
	}
	if s.Attr == "" {
		return nil, errors.New(errors.KindInvalid, "nixman.Load", "spec missing attr")
	}
	switch s.SwitchMode {
	case "", "test", "boot", "switch":
	default:
		return nil, errors.New(errors.KindInvalid, "nixman.Load", "switch_mode must be test|boot|switch")
	}
	return &Resource{id: id, spec: s}, nil
}

func (m *Manager) markerPath(r *Resource) string {
	// Resource IDs contain ':' and '/'; sanitize to a filename.
	safe := make([]byte, 0, len(r.id))
	for _, c := range []byte(r.id) {
		if c == '/' || c == ':' {
			c = '_'
		}
		safe = append(safe, c)
	}
	return filepath.Join(m.markerDir, string(safe))
}

// Observe implements reconcile.Manager. A nix build is far too slow for
// every tick, so sync is judged against the marker recording the system
// path this resource last switched to: in sync iff /run/current-system
// still points at it.
func (m *Manager) Observe(ctx context.Context, r reconcile.Resource) (reconcile.Observed, error) {
	res, ok := r.(*Resource)
	if !ok {
		return reconcile.Observed{}, errors.New(errors.KindInvalid, "nixman.Observe", "wrong resource type")
	}
	cur, err := m.driver.CurrentSystem(ctx)
	if err != nil {
		return reconcile.Observed{}, err
	}
	marker, err := os.ReadFile(m.markerPath(res))
	details := map[string]string{
		"current_system": string(cur),
		"flake":          res.spec.FlakeRef,
		"attr":           res.spec.Attr,
	}
	if err != nil {
		if !os.IsNotExist(err) {
			return reconcile.Observed{}, fmt.Errorf("read marker: %w", err)
		}
		// No marker: this resource has never been switched by the agent.
		details["last_switched_to"] = "(never)"
		return reconcile.Observed{Exists: false, InSync: false, Details: details}, nil
	}
	want := string(marker)
	details["last_switched_to"] = want
	inSync := want == string(cur)
	h := reconcile.HealthDegraded
	if inSync {
		h = reconcile.HealthHealthy
	}
	return reconcile.Observed{Exists: true, InSync: inSync, Health: h, Details: details}, nil
}

// Plan implements reconcile.Manager. The heavy work (build) happens as an
// action, not during planning.
func (m *Manager) Plan(ctx context.Context, r reconcile.Resource, o reconcile.Observed) ([]reconcile.Action, error) {
	res, ok := r.(*Resource)
	if !ok {
		return nil, errors.New(errors.KindInvalid, "nixman.Plan", "wrong resource type")
	}
	if o.InSync {
		return nil, nil
	}
	mode := nix.SwitchMode(res.spec.SwitchMode)
	if mode == "" {
		mode = nix.SwitchDefault
	}
	return []reconcile.Action{{
		ResourceID:  res.id,
		Kind:        "update",
		Description: fmt.Sprintf("nix build %s#%s and switch (%s)", res.spec.FlakeRef, res.spec.Attr, mode),
		Destructive: true,
		Fn: func(ctx context.Context) error {
			return m.buildAndSwitch(ctx, res, mode)
		},
	}}, nil
}

func (m *Manager) buildAndSwitch(ctx context.Context, res *Resource, mode nix.SwitchMode) error {
	path, err := m.driver.Build(ctx, res.spec.FlakeRef, res.spec.Attr, m.logs)
	if err != nil {
		return err
	}
	if err := m.driver.Switch(ctx, path, mode); err != nil {
		return err
	}
	// Record the switched path so future Observe calls are cheap.
	if err := os.MkdirAll(m.markerDir, 0o750); err != nil {
		return err
	}
	return os.WriteFile(m.markerPath(res), []byte(path), 0o644)
}

// Apply implements reconcile.Manager.
func (m *Manager) Apply(ctx context.Context, a reconcile.Action) error {
	if a.Fn == nil {
		return errors.New(errors.KindInternal, "nixman.Apply", "action has no function")
	}
	return a.Fn(ctx)
}
