// Package fileman implements the "file" and "directory" resource managers:
// declarative file/directory convergence with atomic writes.
package fileman

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/reconcile"
	"gopkg.in/yaml.v3"
)

// FileSpec is the desired state of a file resource.
type FileSpec struct {
	Path    string `yaml:"path"`
	Content string `yaml:"content"`
	Mode    string `yaml:"mode"`  // octal, e.g. "0644"; default 0644
	Owner   string `yaml:"owner"` // "user:group", optional (root:root default)
}

// File is a file resource.
type File struct {
	id   string
	spec FileSpec
}

// ID implements reconcile.Resource.
func (f *File) ID() string { return f.id }

// Type implements reconcile.Resource.
func (f *File) Type() string { return "file" }

// Dependencies implements reconcile.Resource.
func (f *File) Dependencies() []string { return nil }

// DirSpec is the desired state of a directory resource.
type DirSpec struct {
	Path  string `yaml:"path"`
	Mode  string `yaml:"mode"`  // octal, e.g. "0755"; default 0755
	Owner string `yaml:"owner"` // "user:group", optional
}

// Dir is a directory resource.
type Dir struct {
	id   string
	spec DirSpec
}

// ID implements reconcile.Resource.
func (d *Dir) ID() string { return d.id }

// Type implements reconcile.Resource.
func (d *Dir) Type() string { return "directory" }

// Dependencies implements reconcile.Resource.
func (d *Dir) Dependencies() []string { return nil }

// Manager handles both "file" and "directory" resources.
type Manager struct{}

// New creates the manager.
func New() *Manager { return &Manager{} }

// Type implements reconcile.Manager (primary type: file).
func (m *Manager) Type() string { return "file" }

// Types lists all resource types this manager handles.
func (m *Manager) Types() []string { return []string{"file", "directory"} }

// FileManager returns a manager restricted to the file type (for registries
// keyed by type).
func (m *Manager) FileManager() reconcile.Manager { return fileOnly{m} }

// DirManager returns a manager restricted to the directory type.
func (m *Manager) DirManager() reconcile.Manager { return dirOnly{m} }

type fileOnly struct{ m *Manager }

func (fileOnly) Type() string { return "file" }
func (f fileOnly) Load(id string, spec []byte) (reconcile.Resource, error) {
	return f.m.loadFile(id, spec)
}

func (f fileOnly) Observe(ctx context.Context, r reconcile.Resource) (reconcile.Observed, error) {
	return f.m.Observe(ctx, r)
}

func (f fileOnly) Plan(ctx context.Context, r reconcile.Resource, o reconcile.Observed) ([]reconcile.Action, error) {
	return f.m.Plan(ctx, r, o)
}

func (f fileOnly) Apply(ctx context.Context, a reconcile.Action) error { return f.m.Apply(ctx, a) }

func (f fileOnly) Delete(ctx context.Context, r reconcile.Resource) error { return f.m.Delete(ctx, r) }

type dirOnly struct{ m *Manager }

func (dirOnly) Type() string { return "directory" }
func (d dirOnly) Load(id string, spec []byte) (reconcile.Resource, error) {
	return d.m.loadDir(id, spec)
}

func (d dirOnly) Observe(ctx context.Context, r reconcile.Resource) (reconcile.Observed, error) {
	return d.m.Observe(ctx, r)
}

func (d dirOnly) Plan(ctx context.Context, r reconcile.Resource, o reconcile.Observed) ([]reconcile.Action, error) {
	return d.m.Plan(ctx, r, o)
}

func (d dirOnly) Apply(ctx context.Context, a reconcile.Action) error { return d.m.Apply(ctx, a) }

func (d dirOnly) Delete(ctx context.Context, r reconcile.Resource) error { return d.m.Delete(ctx, r) }

// Load implements reconcile.Manager for the "file" type.
func (m *Manager) Load(id string, spec []byte) (reconcile.Resource, error) {
	return m.loadFile(id, spec)
}

func (m *Manager) loadFile(id string, spec []byte) (reconcile.Resource, error) {
	var fs FileSpec
	if err := yaml.Unmarshal(spec, &fs); err != nil {
		return nil, errors.Wrap(err, errors.KindInvalid, "fileman.Load", "bad file spec")
	}
	if fs.Path == "" {
		return nil, errors.New(errors.KindInvalid, "fileman.Load", "file spec missing path")
	}
	if _, err := parseMode(fs.Mode, 0o644); err != nil {
		return nil, err
	}
	return &File{id: id, spec: fs}, nil
}

func (m *Manager) loadDir(id string, spec []byte) (reconcile.Resource, error) {
	var ds DirSpec
	if err := yaml.Unmarshal(spec, &ds); err != nil {
		return nil, errors.Wrap(err, errors.KindInvalid, "dirman.Load", "bad directory spec")
	}
	if ds.Path == "" {
		return nil, errors.New(errors.KindInvalid, "dirman.Load", "directory spec missing path")
	}
	return &Dir{id: id, spec: ds}, nil
}

// Observe implements reconcile.Manager.
func (m *Manager) Observe(ctx context.Context, r reconcile.Resource) (reconcile.Observed, error) {
	switch res := r.(type) {
	case *File:
		return observeFile(res)
	case *Dir:
		return observeDir(res)
	default:
		return reconcile.Observed{}, errors.New(errors.KindInvalid, "fileman.Observe", "unsupported resource type")
	}
}

func parseMode(s string, def os.FileMode) (os.FileMode, error) {
	if s == "" {
		return def, nil
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, errors.Wrap(err, errors.KindInvalid, "fileman", "bad octal mode")
	}
	return os.FileMode(v), nil
}

func observeFile(f *File) (reconcile.Observed, error) {
	wantMode, err := parseMode(f.spec.Mode, 0o644)
	if err != nil {
		return reconcile.Observed{}, err
	}
	st, err := os.Lstat(f.spec.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return reconcile.Observed{Exists: false, InSync: false, Details: map[string]string{"path": f.spec.Path}}, nil
		}
		return reconcile.Observed{}, errors.Wrap(err, errors.KindInternal, "fileman.Observe", "lstat failed")
	}
	if st.IsDir() {
		return reconcile.Observed{
			Exists: true, InSync: false, Health: reconcile.HealthUnhealthy,
			Details: map[string]string{"path": f.spec.Path, "problem": "is a directory"},
		}, nil
	}
	data, err := os.ReadFile(f.spec.Path)
	if err != nil {
		return reconcile.Observed{}, errors.Wrap(err, errors.KindInternal, "fileman.Observe", "read failed")
	}
	inSync := string(data) == f.spec.Content && st.Mode().Perm() == wantMode
	return reconcile.Observed{
		Exists: true,
		InSync: inSync,
		Health: healthFromSync(inSync),
		Details: map[string]string{
			"path":     f.spec.Path,
			"sha256":   fmt.Sprintf("%x", sha256.Sum256(data)),
			"mode":     fmt.Sprintf("%04o", st.Mode().Perm()),
			"wantMode": fmt.Sprintf("%04o", wantMode),
		},
	}, nil
}

func observeDir(d *Dir) (reconcile.Observed, error) {
	wantMode, err := parseMode(d.spec.Mode, 0o755)
	if err != nil {
		return reconcile.Observed{}, err
	}
	st, err := os.Lstat(d.spec.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return reconcile.Observed{Exists: false, InSync: false, Details: map[string]string{"path": d.spec.Path}}, nil
		}
		return reconcile.Observed{}, errors.Wrap(err, errors.KindInternal, "dirman.Observe", "lstat failed")
	}
	if !st.IsDir() {
		return reconcile.Observed{
			Exists: true, InSync: false, Health: reconcile.HealthUnhealthy,
			Details: map[string]string{"path": d.spec.Path, "problem": "not a directory"},
		}, nil
	}
	inSync := st.Mode().Perm() == wantMode
	return reconcile.Observed{
		Exists:  true,
		InSync:  inSync,
		Health:  healthFromSync(inSync),
		Details: map[string]string{"path": d.spec.Path, "mode": fmt.Sprintf("%04o", st.Mode().Perm())},
	}, nil
}

func healthFromSync(inSync bool) reconcile.Health {
	if inSync {
		return reconcile.HealthHealthy
	}
	return reconcile.HealthDegraded
}

// Plan implements reconcile.Manager.
func (m *Manager) Plan(ctx context.Context, r reconcile.Resource, o reconcile.Observed) ([]reconcile.Action, error) {
	switch res := r.(type) {
	case *File:
		return planFile(res, o)
	case *Dir:
		return planDir(res, o)
	default:
		return nil, errors.New(errors.KindInvalid, "fileman.Plan", "unsupported resource type")
	}
}

func planFile(f *File, o reconcile.Observed) ([]reconcile.Action, error) {
	wantMode, err := parseMode(f.spec.Mode, 0o644)
	if err != nil {
		return nil, err
	}
	var actions []reconcile.Action
	if !o.Exists || contentDrift(o, f.spec.Content) {
		actions = append(actions, reconcile.Action{
			ResourceID:  f.id,
			Kind:        "create",
			Description: fmt.Sprintf("write %s (%d bytes, mode %04o)", f.spec.Path, len(f.spec.Content), wantMode),
			Fn:          func(ctx context.Context) error { return atomicWrite(f.spec.Path, []byte(f.spec.Content), wantMode) },
		})
	} else if modeDrift(o, wantMode) {
		actions = append(actions, reconcile.Action{
			ResourceID:  f.id,
			Kind:        "update",
			Description: fmt.Sprintf("chmod %s to %04o", f.spec.Path, wantMode),
			Fn:          func(ctx context.Context) error { return os.Chmod(f.spec.Path, wantMode) },
		})
	}
	return actions, nil
}

func contentDrift(o reconcile.Observed, want string) bool {
	got, ok := o.Details["sha256"]
	if !ok {
		return true // not observed yet (or missing)
	}
	return got != fmt.Sprintf("%x", sha256.Sum256([]byte(want)))
}

func modeDrift(o reconcile.Observed, want os.FileMode) bool {
	return o.Details["mode"] != fmt.Sprintf("%04o", want)
}

func planDir(d *Dir, o reconcile.Observed) ([]reconcile.Action, error) {
	wantMode, err := parseMode(d.spec.Mode, 0o755)
	if err != nil {
		return nil, err
	}
	var actions []reconcile.Action
	if !o.Exists {
		actions = append(actions, reconcile.Action{
			ResourceID:  d.id,
			Kind:        "create",
			Description: fmt.Sprintf("mkdir -p %s (mode %04o)", d.spec.Path, wantMode),
			Fn: func(ctx context.Context) error {
				if err := os.MkdirAll(d.spec.Path, wantMode); err != nil {
					return err
				}
				return os.Chmod(d.spec.Path, wantMode) // MkdirAll is affected by umask
			},
		})
	} else if modeDrift(o, wantMode) {
		actions = append(actions, reconcile.Action{
			ResourceID:  d.id,
			Kind:        "update",
			Description: fmt.Sprintf("chmod %s to %04o", d.spec.Path, wantMode),
			Fn:          func(ctx context.Context) error { return os.Chmod(d.spec.Path, wantMode) },
		})
	}
	return actions, nil
}

// Apply implements reconcile.Manager.
func (m *Manager) Apply(ctx context.Context, a reconcile.Action) error {
	if a.Fn == nil {
		return errors.New(errors.KindInternal, "fileman.Apply", "action has no function")
	}
	return a.Fn(ctx)
}

// Delete implements reconcile.Deleter: remove the file (or directory) the
// resource manages. Only top-level managed paths are removed; the path
// came from desired state, so this is the declarative inverse of create.
func (m *Manager) Delete(ctx context.Context, r reconcile.Resource) error {
	switch res := r.(type) {
	case *File:
		if err := os.Remove(res.spec.Path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", res.spec.Path, err)
		}
		return nil
	case *Dir:
		// Only remove if empty — never recurse; the directory may hold
		// unrelated state.
		if err := os.Remove(res.spec.Path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w (must be empty)", res.spec.Path, err)
		}
		return nil
	default:
		return errors.New(errors.KindInvalid, "fileman.Delete", "unsupported resource type")
	}
}

// atomicWrite writes data to path via a temp file + fsync + rename, so
// readers never observe a torn write. On failure no temp file is left
// behind.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".expanse-tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil { // fsync the data before rename
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
