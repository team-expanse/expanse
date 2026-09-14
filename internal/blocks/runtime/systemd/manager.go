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
	unit := UnitName(res.spec.Namespace, res.spec.Name, res.spec.Index)
	load, active, sub, err := m.API.UnitState(ctx, unit)
	if err != nil {
		return reconcile.Observed{}, errors.Wrap(err, errors.KindInternal, "systemd.Observe", "unit state")
	}
	inSync := load != "not-found" && active == "active" && sub != "failed"
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
	unit := UnitName(res.spec.Namespace, res.spec.Name, res.spec.Index)
	spec := res.spec
	var acts []reconcile.Action
	acts = append(acts, reconcile.Action{
		ResourceID:  res.id,
		Kind:        "update",
		Description: "resolve block closure (cache-checked build)",
		Fn: func(ctx context.Context) error {
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

// apply resolves the closure through the cache.
func (m *Manager) apply(ctx context.Context, s Spec) (ApplyResult, bool, error) {
	res, err := m.Applier.Apply(ctx, s)
	if err != nil {
		return ApplyResult{}, false, errors.Wrap(err, errors.KindUnavailable, "systemd.Apply", "closure")
	}
	return res, res.Built, nil
}
