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
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/expanse/expanse/internal/blocks/livespec"
	"github.com/expanse/expanse/internal/blocks/vm/vmstate"
	"github.com/expanse/expanse/internal/blocks/vmready"
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
	// StatusText is the unit's sd_notify STATUS= text (empty when never sent).
	StatusText(ctx context.Context, unit string) (string, error)
	// Reload sends the unit's main process SIGHUP: re-read the spec and apply its live fields.
	Reload(ctx context.Context, unit string) error
}

// reportsReadiness lists block types whose runtime publishes readiness as the unit's status text.
var reportsReadiness = map[string]bool{"vm/instance": true}

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
	// SecretDir holds each replica's unsealed secrets, one file per config field, which
	// the unit loads with LoadCredential; empty = block-secrets beside SpecDir.
	SecretDir string
	// Unseal reads one of a namespace's block secrets; nil delivers none.
	Unseal func(ctx context.Context, ns, name string) ([]byte, error)
	// StateDir holds suspended VMs' saved state; empty = vmstate.DefaultDir.
	StateDir string
	// KeepDir holds the requests that a VM's next stop save its guest; empty = vmstate.DefaultKeepDir.
	KeepDir string
	// LiveDir holds the markers of runtimes that serve reloads; empty = livespec.DefaultDir.
	LiveDir string
	// Mounted reports whether a volume mount's host path is mounted; a replica starts only
	// once all of its are. nil skips the check.
	Mounted func(path string) (bool, error)
}

// DefaultSpecDir is the runtime directory expanse-block-run reads its
// per-replica spec from (the static template unit only carries %i).
const DefaultSpecDir = "/run/expanse/block-replica"

// secretDir is the replica's credential directory: under SecretDir, by default
// block-secrets beside the spec directory (/run/expanse/block-secrets, tmpfs).
func (m *Manager) secretDir(s Spec) string {
	base := m.SecretDir
	if base == "" {
		base = filepath.Join(filepath.Dir(m.specDir()), "block-secrets")
	}
	return filepath.Join(base, Instance(s.Namespace, s.Name, s.Index))
}

// writeSecrets unseals the replica's secrets into its credential directory and drops
// stale ones. The directory always exists: the unit's LoadCredential fails without it.
func (m *Manager) writeSecrets(ctx context.Context, s Spec) error {
	const op = "systemd.writeSecrets"
	dir := m.secretDir(s)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.Wrap(err, errors.KindInternal, op, dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return errors.Wrap(err, errors.KindInternal, op, dir)
	}
	for field, name := range s.Secrets {
		if field == "" || strings.ContainsAny(field, "/\\") || strings.HasPrefix(field, ".") {
			return errors.New(errors.KindInvalid, op, fmt.Sprintf("bad secret field %q", field))
		}
		if m.Unseal == nil {
			return errors.New(errors.KindUnavailable, op, "this node cannot unseal secrets")
		}
		v, err := m.Unseal(ctx, s.Namespace, name)
		if err != nil {
			return errors.Wrap(err, errors.KindOf(err), op, "secret "+name)
		}
		if err := writeFileSync(filepath.Join(dir, field), v, 0o600); err != nil {
			return errors.Wrap(err, errors.KindInternal, op, field)
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return errors.Wrap(err, errors.KindInternal, op, dir)
	}
	for _, e := range ents {
		if _, ok := s.Secrets[e.Name()]; !ok {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	return nil
}

// secretsCurrent reports whether the credentials on disk were written for this spec.
func (m *Manager) secretsCurrent(s Spec) bool {
	_, err := os.Stat(m.secretDir(s))
	return err == nil && m.specOnDiskMatches(s)
}

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

// specOnDisk reads the spec file the running unit started from (or last reloaded).
func (m *Manager) specOnDisk(s Spec) (Spec, bool) {
	path := filepath.Join(m.specDir(), Instance(s.Namespace, s.Name, s.Index)+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, false
	}
	var onDisk Spec
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		return Spec{}, false
	}
	return onDisk, true
}

// specOnDiskMatches reports whether the spec file the running unit reads equals the desired spec.
// Config JSON compares by content, so an agent build that encodes it differently is no drift.
func (m *Manager) specOnDiskMatches(s Spec) bool {
	onDisk, ok := m.specOnDisk(s)
	return ok && sameSpec(onDisk, s)
}

func sameSpec(a, b Spec) bool {
	a.Args, b.Args = livespec.CanonicalArgs(a.Args), livespec.CanonicalArgs(b.Args)
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}

// liveMarker is where the replica's runtime says it serves reloads.
func (m *Manager) liveMarker(s Spec) string {
	return livespec.MarkerPath(m.LiveDir, Instance(s.Namespace, s.Name, s.Index))
}

// reloadable reports whether the running unit can take s by a reload: its runtime serves them,
// and s differs from the spec it runs only in its type's live fields.
func (m *Manager) reloadable(s Spec, o reconcile.Observed) bool {
	if s.Stopped || o.Details["active"] != "active" || o.Details["sub"] != "running" {
		return false
	}
	if _, err := os.Stat(m.liveMarker(s)); err != nil {
		return false
	}
	cur, ok := m.specOnDisk(s)
	if !ok || cur.Stopped || !liveArgs(s.Type, cur.Args, s.Args) {
		return false
	}
	// A running VM reads its state token and placement only when it starts or stops.
	rest, next := cur, s
	rest.Args, next.Args = nil, nil
	rest.StateToken, next.StateToken = "", ""
	rest.Placement, next.Placement = "", ""
	return sameSpec(rest, next)
}

// liveArgs reports whether next's args are cur's, or differ only in typ's live fields.
func liveArgs(typ string, cur, next []string) bool {
	return slices.Equal(livespec.CanonicalArgs(cur), livespec.CanonicalArgs(next)) || livespec.LiveOnly(typ, cur, next)
}

// keepsGuest reports whether s asks the running VM to move onto the node's current runtime: a new
// update request, maybe with a resize, which its resumed run applies.
func (m *Manager) keepsGuest(s Spec, o reconcile.Observed) bool {
	if s.Type != "vm/instance" || s.Stopped || s.Placement == "" ||
		o.Details["active"] != "active" || o.Details["sub"] != "running" {
		return false
	}
	cur, ok := m.specOnDisk(s)
	if !ok || cur.Stopped || cur.Refresh == s.Refresh || !liveArgs(s.Type, cur.Args, s.Args) {
		return false
	}
	rest, next := cur, s
	rest.Args, next.Args = nil, nil
	rest.StateToken, next.StateToken = "", ""
	rest.Refresh, next.Refresh = "", ""
	return sameSpec(rest, next)
}

// keepMarker is where the agent asks the replica's next stop to save its guest.
func (m *Manager) keepMarker(s Spec) string {
	return vmstate.KeepPath(m.KeepDir, Instance(s.Namespace, s.Name, s.Index))
}

// setKeep asks the replica's next stop to save its guest, or withdraws the request.
func (m *Manager) setKeep(s Spec, keep bool) error {
	path := m.keepMarker(s)
	if !keep {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return errors.Wrap(err, errors.KindInternal, "systemd.setKeep", path)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return errors.Wrap(err, errors.KindInternal, "systemd.setKeep", path)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return errors.Wrap(err, errors.KindInternal, "systemd.setKeep", path)
	}
	return nil
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
	if res.spec.Stopped {
		return m.observeStopped(res.spec, unit, load, active, sub), nil
	}
	missing, err := m.missingMount(res.spec)
	if err != nil {
		return reconcile.Observed{}, errors.Wrap(err, errors.KindInternal, "systemd.Observe", "volume mounts")
	}
	if missing != "" {
		ready := false
		return reconcile.Observed{
			Exists: load != "not-found",
			Health: reconcile.HealthDegraded,
			Ready:  &ready,
			Details: map[string]string{
				"unit": unit, "load": load, "active": active, "sub": sub,
				"status": "waiting for volume mount " + missing,
			},
		}, nil
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
	obs := reconcile.Observed{
		Exists: load != "not-found",
		InSync: inSync,
		Health: h,
		Details: map[string]string{
			"unit": unit, "load": load, "active": active, "sub": sub,
		},
	}
	if inSync && reportsReadiness[res.spec.Type] {
		text, err := m.API.StatusText(ctx, unit)
		ready := err == nil && vmready.StatusTextReady(text) // unreadable is not ready; nothing demotes on it
		obs.Ready = &ready
		obs.Details["status"] = text
	}
	return obs, nil
}

// observeStopped reports a replica meant to be down: in sync once its unit is, with "suspended
// <token>" when this node holds the save its spec names, else "stopped".
func (m *Manager) observeStopped(s Spec, unit, load, active, sub string) reconcile.Observed {
	down := active == "inactive" || active == "failed"
	inSync := down && m.specOnDiskMatches(s)
	h := reconcile.HealthDegraded
	if inSync {
		h = reconcile.HealthHealthy
	}
	status := "stopped"
	if tok := vmstate.Token(m.stateDir(s)); tok != "" && tok == s.StateToken {
		status = "suspended " + tok
	}
	return reconcile.Observed{
		Exists: load != "not-found",
		InSync: inSync,
		Health: h,
		Details: map[string]string{
			"unit": unit, "load": load, "active": active, "sub": sub, "status": status,
		},
	}
}

// stateDir is the replica's saved-state directory.
func (m *Manager) stateDir(s Spec) string {
	return vmstate.Dir(m.StateDir, Instance(s.Namespace, s.Name, s.Index))
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
	// Without its volume mounts the workload would write to the node's own disk.
	if missing, err := m.missingMount(spec); err != nil || missing != "" {
		return nil, err
	}
	var acts []reconcile.Action
	acts = append(acts, reconcile.Action{
		ResourceID:  res.id,
		Kind:        "update",
		Description: "resolve block closure (cache-checked build)",
		Fn: func(ctx context.Context) error {
			if !m.secretsCurrent(spec) {
				if err := m.writeSecrets(ctx, spec); err != nil {
					return err
				}
			}
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
	if !o.InSync && m.reloadable(spec, o) {
		acts = append(acts, reconcile.Action{
			ResourceID:  res.id,
			Kind:        "reload",
			Description: "reload " + unit + " (live fields only)",
			Fn: func(ctx context.Context) error {
				return m.API.Reload(ctx, unit)
			},
		})
	} else if !o.InSync && spec.Stopped {
		acts = append(acts, reconcile.Action{
			ResourceID:  res.id,
			Kind:        "stop",
			Description: "stop " + unit + " (suspended)",
			Fn: func(ctx context.Context) error {
				return m.API.Stop(ctx, unit)
			},
		})
	} else if !o.InSync {
		keep := m.keepsGuest(spec, o)
		desc := "start " + unit
		if keep {
			desc = "restart " + unit + " onto the node's runtime, keeping its guest"
		}
		acts = append(acts, reconcile.Action{
			ResourceID:  res.id,
			Kind:        "start",
			Description: desc,
			Fn: func(ctx context.Context) error {
				// The new run writes its own marker once it serves reloads.
				if err := os.Remove(m.liveMarker(spec)); err != nil && !os.IsNotExist(err) {
					return errors.Wrap(err, errors.KindInternal, "systemd.Start", "reload marker")
				}
				if err := m.setKeep(spec, keep); err != nil {
					return err
				}
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
	// A deleted VM powers off, never saves.
	if err := m.setKeep(res.spec, false); err != nil {
		return err
	}
	_ = m.API.Stop(ctx, UnitNameForSpec(res.spec))
	if err := os.RemoveAll(m.secretDir(res.spec)); err != nil {
		return errors.Wrap(err, errors.KindInternal, "systemd.Delete", "credentials")
	}
	if err := os.Remove(m.liveMarker(res.spec)); err != nil && !os.IsNotExist(err) {
		return errors.Wrap(err, errors.KindInternal, "systemd.Delete", "reload marker")
	}
	if err := vmstate.Remove(m.stateDir(res.spec)); err != nil {
		return errors.Wrap(err, errors.KindInternal, "systemd.Delete", "saved state")
	}
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
