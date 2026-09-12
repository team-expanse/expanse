// Package sysctlman converges kernel parameters under /proc/sys.
// Runtime-only: persistence across reboots is the nix-config manager's job.
package sysctlman

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/reconcile"
	"gopkg.in/yaml.v3"
)

// Spec is the desired state of a sysctl resource.
type Spec struct {
	Key   string `yaml:"key"`   // dotted, e.g. "net.core.somaxconn"
	Value string `yaml:"value"` // desired value, e.g. "4096"
}

// Resource is a sysctl resource.
type Resource struct {
	id   string
	spec Spec
}

// ID implements reconcile.Resource.
func (r *Resource) ID() string { return r.id }

// Type implements reconcile.Resource.
func (r *Resource) Type() string { return "sysctl" }

// Dependencies implements reconcile.Resource.
func (r *Resource) Dependencies() []string { return nil }

// Manager handles "sysctl" resources.
type Manager struct{}

// New creates the manager.
func New() *Manager { return &Manager{} }

// Type implements reconcile.Manager.
func (m *Manager) Type() string { return "sysctl" }

// procRoot is the sysctl tree root; /proc/sys in production (overridable
// in tests).
var procRoot = "/proc/sys"

// procPath maps a dotted sysctl key to its path under procRoot.
func procPath(key string) string {
	return procRoot + "/" + strings.ReplaceAll(key, ".", "/")
}

// Load implements reconcile.Manager.
func (m *Manager) Load(id string, spec []byte) (reconcile.Resource, error) {
	var s Spec
	if err := yaml.Unmarshal(spec, &s); err != nil {
		return nil, errors.Wrap(err, errors.KindInvalid, "sysctlman.Load", "bad spec")
	}
	if s.Key == "" {
		return nil, errors.New(errors.KindInvalid, "sysctlman.Load", "spec missing key")
	}
	if s.Value == "" {
		return nil, errors.New(errors.KindInvalid, "sysctlman.Load", "spec missing value")
	}
	return &Resource{id: id, spec: s}, nil
}

// Observe implements reconcile.Manager.
func (m *Manager) Observe(ctx context.Context, r reconcile.Resource) (reconcile.Observed, error) {
	res, ok := r.(*Resource)
	if !ok {
		return reconcile.Observed{}, errors.New(errors.KindInvalid, "sysctlman.Observe", "wrong resource type")
	}
	data, err := os.ReadFile(procPath(res.spec.Key))
	if err != nil {
		if os.IsNotExist(err) {
			return reconcile.Observed{
				Exists: false, InSync: false,
				Details: map[string]string{"key": res.spec.Key, "problem": "no such sysctl"},
			}, nil
		}
		return reconcile.Observed{}, errors.Wrap(err, errors.KindPermission, "sysctlman.Observe", "read /proc/sys")
	}
	got := normalizeSysctl(string(data))
	want := normalizeSysctl(res.spec.Value)
	inSync := got == want
	h := reconcile.HealthDegraded
	if inSync {
		h = reconcile.HealthHealthy
	}
	return reconcile.Observed{
		Exists:  true,
		InSync:  inSync,
		Health:  h,
		Details: map[string]string{"key": res.spec.Key, "value": got, "want": want},
	}, nil
}

// normalizeSysctl trims whitespace and collapses internal whitespace
// separators (some sysctls return tab-separated lists).
func normalizeSysctl(v string) string {
	fields := strings.Fields(v)
	return strings.Join(fields, " ")
}

// Plan implements reconcile.Manager.
func (m *Manager) Plan(ctx context.Context, r reconcile.Resource, o reconcile.Observed) ([]reconcile.Action, error) {
	res, ok := r.(*Resource)
	if !ok {
		return nil, errors.New(errors.KindInvalid, "sysctlman.Plan", "wrong resource type")
	}
	if o.InSync {
		return nil, nil
	}
	return []reconcile.Action{{
		ResourceID:  res.id,
		Kind:        "update",
		Description: fmt.Sprintf("sysctl %s=%s", res.spec.Key, res.spec.Value),
		Fn: func(ctx context.Context) error {
			return os.WriteFile(procPath(res.spec.Key), []byte(res.spec.Value), 0o644)
		},
	}}, nil
}

// Apply implements reconcile.Manager.
func (m *Manager) Apply(ctx context.Context, a reconcile.Action) error {
	if a.Fn == nil {
		return errors.New(errors.KindInternal, "sysctlman.Apply", "action has no function")
	}
	return a.Fn(ctx)
}
