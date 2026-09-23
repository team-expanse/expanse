// Manager wiring: a reconcile.Manager for "block-replica" resources so
// the agent's existing reconcile loop (Phase 02) converges block replicas
// on its node. Desired-state spec = JSON-encoded Spec. The manager:
//
//   - Observe: queries the unit's state via the injectable unit API.
//   - Plan: build-then-ensure-start. The closure cache short-circuits the
//     build step when the hash is unchanged (§5.3 < 5 s replicas-only
//     change); unit state is always touched.
//   - Apply: executes the planned actions (build/switch + start/stop).
package systemd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/reconcile"
)

// TypeBlockReplica is the reconcile resource type this manager handles.
const TypeBlockReplica = "block-replica"

// unitAPI is the subset of systemd control the manager needs (mirrors
// systemdman's interface style; the production implementation goes over
// D-Bus, tests inject fakes).
type unitAPI interface {
	// UnitState returns the unit's load/active/sub state triple.
	UnitState(ctx context.Context, unit string) (load, active, sub string, err error)
	Start(ctx context.Context, unit string) error
	Stop(ctx context.Context, unit string) error
}

// Resource is the loaded desired state for one replica.
type Resource struct {
	id   string
	spec Spec
}

// ID implements reconcile.Resource.
func (r *Resource) ID() string { return r.id }

// Type implements reconcile.Resource.
func (r *Resource) Type() string { return TypeBlockReplica }

// Dependencies implements reconcile.Resource.
func (r *Resource) Dependencies() []string { return nil }

// Manager converges block-replica resources.
type Manager struct {
	// API controls units on this node.
	API unitAPI
	// Applier resolves closures through the cache.
	Applier *Applier
	// Switch, when set, runs after a real build (nix driver switch).
	// nil in dry-run/test.
	Switch func(ctx context.Context, storePath string) error
	// SpecDir is where per-replica desired-state JSON is written for
	// expanse-block-run (default DefaultSpecDir); empty = default.
	SpecDir string
}

// DefaultSpecDir is the runtime directory expanse-block-run reads its
// per-replica spec from (the static template unit only carries %i).
const DefaultSpecDir = "/run/expanse/block-replica"

// specDir returns the configured spec directory.
func (m *Manager) specDir() string {
	if m.SpecDir != "" {
		return m.SpecDir
	}
	return DefaultSpecDir
}

// writeSpec persists the per-replica spec JSON for the helper binary.
func (m *Manager) writeSpec(s Spec) error {
	if err := os.MkdirAll(m.specDir(), 0o755); err != nil {
		return errors.Wrap(err, errors.KindInternal, "systemd.writeSpec", m.specDir())
	}
	out, err := json.Marshal(s)
	if err != nil {
		return errors.Wrap(err, errors.KindInternal, "systemd.writeSpec", "marshal")
	}
	path := filepath.Join(m.specDir(), Instance(s.Namespace, s.Name, s.Index)+".json")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return errors.Wrap(err, errors.KindInternal, "systemd.writeSpec", path)
	}
	return nil
}

// specOnDiskMatches reports whether the spec file the running unit
// reads equals the desired spec (canonical JSON comparison). A missing
// file matches nothing — the unit has never been written for.
func (m *Manager) specOnDiskMatches(s Spec) bool {
	path := filepath.Join(m.specDir(), Instance(s.Namespace, s.Name, s.Index)+".json")
	onDisk, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	want, err := json.Marshal(s)
	if err != nil {
		return false
	}
	return string(onDisk) == string(want)
}

// removeSpec drops the per-replica spec JSON (resource deletion).
func (m *Manager) removeSpec(s Spec) error {
	path := filepath.Join(m.specDir(), Instance(s.Namespace, s.Name, s.Index)+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return errors.Wrap(err, errors.KindInternal, "systemd.removeSpec", path)
	}
	return nil
}

// NewManager wires a Manager.
func NewManager(api unitAPI, applier *Applier) *Manager {
	return &Manager{API: api, Applier: applier}
}

// Type implements reconcile.Manager.
func (m *Manager) Type() string { return TypeBlockReplica }

// Load implements reconcile.Manager: decodes a JSON Spec.
func (m *Manager) Load(id string, spec []byte) (reconcile.Resource, error) {
	var s Spec
	if err := json.Unmarshal(spec, &s); err != nil {
		return nil, errors.Wrap(err, errors.KindInvalid, "systemd.Load", "bad block-replica spec")
	}
	if s.Namespace == "" || s.Name == "" {
		return nil, errors.New(errors.KindInvalid, "systemd.Load", "spec missing namespace/name")
	}
	return &Resource{id: id, spec: s}, nil
}

// Observe implements reconcile.Manager.
func (m *Manager) Observe(ctx context.Context, r reconcile.Resource) (reconcile.Observed, error) {
	res, ok := r.(*Resource)
	if !ok {
		return reconcile.Observed{}, errors.New(errors.KindInvalid, "systemd.Observe", "wrong resource type")
	}
	unit := UnitNameForSpec(res.spec)
	load, active, sub, err := m.API.UnitState(ctx, unit)
	if err != nil {
		return reconcile.Observed{}, errors.Wrap(err, errors.KindInternal, "systemd.Observe", "unit state")
	}
	inSync := load != "not-found" && active == "active" && sub != "failed" &&
		// Spec drift (e.g. a §5.2 in-place config change): the unit can
		// be active while running the OLD config — the runtime helper
		// reads the spec file only at startup, so a mismatch means the
		// unit must be restarted. Detect by comparing the on-disk spec
		// the unit was started with against the desired spec.
		m.specOnDiskMatches(res.spec)
	h := reconcile.HealthDegraded
	if inSync {
		h = reconcile.HealthHealthy
	}
	return reconcile.Observed{
		Exists: load != "not-found",
		InSync: inSync,
		Health: h,
		Details: map[string]string{
			"unit": unit, "load": load, "active": active, "sub": sub,
		},
	}, nil
}

// Plan implements reconcile.Manager: resolve the closure (cache-checked)
// and ensure the unit is running.
func (m *Manager) Plan(ctx context.Context, r reconcile.Resource, o reconcile.Observed) ([]reconcile.Action, error) {
	res, ok := r.(*Resource)
	if !ok {
		return nil, errors.New(errors.KindInvalid, "systemd.Plan", "wrong resource type")
	}
	unit := UnitNameForSpec(res.spec)
	spec := res.spec
	var acts []reconcile.Action
	acts = append(acts, reconcile.Action{
		ResourceID:  res.id,
		Kind:        "update",
		Description: "resolve block closure (cache-checked build)",
		Fn: func(ctx context.Context) error {
			if err := m.writeSpec(spec); err != nil {
				return err
			}
			res2, built, err := m.apply(ctx, spec)
			if err != nil {
				return err
			}
			if built && m.Switch != nil {
				if err := m.Switch(ctx, res2.StorePath); err != nil {
					return err
				}
			}
			return nil
		},
	})
	if !o.InSync {
		acts = append(acts, reconcile.Action{
			ResourceID:  res.id,
			Kind:        "start",
			Description: "start " + unit,
			Fn: func(ctx context.Context) error {
				return m.API.Start(ctx, unit)
			},
		})
	}
	return acts, nil
}

// Apply implements reconcile.Manager.
func (m *Manager) Apply(ctx context.Context, a reconcile.Action) error {
	return a.Fn(ctx)
}

// Delete implements reconcile.Deleter: stop the unit and drop its spec
// file (removal of the desired state removes the replica).
func (m *Manager) Delete(ctx context.Context, r reconcile.Resource) error {
	res, ok := r.(*Resource)
	if !ok {
		return errors.New(errors.KindInvalid, "systemd.Delete", "wrong resource type")
	}
	// Best-effort stop: the unit may never have started (desired state
	// removed before the first converge). A stale unit that keeps
	// running on a failed stop is caught by the next pass; the spec
	// file removal is strict — it decides expanse-block-run's behavior.
	_ = m.API.Stop(ctx, UnitNameForSpec(res.spec))
	return m.removeSpec(res.spec)
}

// apply resolves the closure through the cache.
func (m *Manager) apply(ctx context.Context, s Spec) (ApplyResult, bool, error) {
	res, err := m.Applier.Apply(ctx, s)
	if err != nil {
		return ApplyResult{}, false, errors.Wrap(err, errors.KindUnavailable, "systemd.Apply", "closure")
	}
	return res, res.Built, nil
}
