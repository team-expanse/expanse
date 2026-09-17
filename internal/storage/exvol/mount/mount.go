// Package mount is the agent-side exvol volume attach/mount resource
// (Phase 06 T14, §4.7): wait for /dev/exvol/<id> (the runtime attaches
// the device when the node is primary), format ONLY an unformatted
// device (never reformat an existing filesystem — a hard rule, checked
// with blkid first), mount at the host path with noatime, and expose
// the binding (BindPaths=/ReadWritePaths=) for the block's unit.
//
// Detach: unmount gracefully, falling back to `umount -l` after a
// timeout (§4.7's ordering hazard — the block's unit must stop BEFORE
// the device detaches, enforced by unit ordering; the mount manager
// never detaches out from under a running unit — closing the device
// and releasing the primary lease are the volume runtime's job when
// the controller moves the primary).
package mount

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/reconcile"
)

// AttachTimeout bounds the wait for /dev/exvol/<id> after placement
// (§4.7 step 3: 30 s).
const AttachTimeout = 30 * time.Second

// DefaultBase is the host mount root for volume devices.
const DefaultBase = "/var/lib/expanse/volumes"

// Resource is the desired state of one volume's local mount.
type Resource struct {
	VolID      string `json:"volId"`
	Name       string `json:"name,omitempty"`
	MountPath  string `json:"mountPath"`
	Filesystem string `json:"filesystem,omitempty"` // ext4 | xfs | none
}

// Manager converges volume-attach resources.
type Manager struct {
	Base string // host mount root (default DefaultBase)
	Run  Runner // command runner (ExecRunner in prod)
	// Wait bounds the device wait (default AttachTimeout).
	Wait time.Duration
}

// Runner runs a command and returns its combined output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner shells out for real.
type ExecRunner struct{}

// Run implements Runner with os/exec.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// HostPath is the canonical host mount point for a volume's device.
// Deterministic — the leader's bridge computes the same path when it
// wires the block's unit bindings.
func HostPath(base, volID string) string {
	if base == "" {
		base = DefaultBase
	}
	return base + "/" + volID + "/mnt"
}

type attachRes struct {
	id   string
	spec Resource
}

func (a *attachRes) ID() string   { return a.id }
func (a *attachRes) Type() string { return Type }
func (a *attachRes) Dependencies() []string {
	// The device must exist before mount: the volume runtime's
	// attach (T09/T10) is not a reconciler resource, so the mount
	// manager waits for it itself (AttachTimeout).
	return nil
}

// Type is the reconcile resource type this manager handles.
const Type = "exvol-attach"

// New builds the manager.
func New(runner Runner, base string) *Manager {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Manager{Base: base, Run: runner, Wait: AttachTimeout}
}

// Type implements reconcile.Manager.
func (m *Manager) Type() string { return Type }

// Load implements reconcile.Manager.
func (m *Manager) Load(id string, spec []byte) (reconcile.Resource, error) {
	var r Resource
	if err := json.Unmarshal(spec, &r); err != nil {
		return nil, fmt.Errorf("exvol-attach %s: decode: %w", id, err)
	}
	if r.VolID == "" {
		return nil, fmt.Errorf("exvol-attach %s: volId required", id)
	}
	if r.MountPath == "" {
		return nil, fmt.Errorf("exvol-attach %s: mountPath required", id)
	}
	return &attachRes{id: id, spec: r}, nil
}

// Observe implements reconcile.Manager: synced when the device is
// mounted at the host path.
func (m *Manager) Observe(ctx context.Context, r reconcile.Resource) (reconcile.Observed, error) {
	a := r.(*attachRes)
	host := HostPath(m.Base, a.spec.VolID)
	o := reconcile.Observed{Details: map[string]string{}, Health: reconcile.HealthHealthy}
	out, err := m.Run.Run(ctx, "findmnt", "-rn", "-S", "/dev/exvol/"+a.spec.VolID, "-n", "-o", "TARGET")
	if err == nil && strings.TrimSpace(out) == host {
		o.Exists = true
		o.InSync = true
		return o, nil
	}
	// Unmounted: still "exists" (the desired object is the mount,
	// which we can always create once the device shows up).
	o.Exists = true
	o.Health = reconcile.HealthDegraded
	return o, nil
}

// Plan implements reconcile.Manager.
func (m *Manager) Plan(ctx context.Context, r reconcile.Resource, o reconcile.Observed) ([]reconcile.Action, error) {
	a := r.(*attachRes)
	if o.InSync {
		return nil, nil
	}
	return []reconcile.Action{{
		ResourceID:  a.ID(),
		Kind:        "update",
		Description: fmt.Sprintf("attach %s (format only if blank, mount at %s, noatime)", a.spec.VolID, a.spec.MountPath),
		Fn: func(ctx context.Context) error {
			return m.attach(ctx, a.spec)
		},
	}}, nil
}

// Apply implements reconcile.Manager.
func (m *Manager) Apply(ctx context.Context, a reconcile.Action) error {
	if a.Fn == nil {
		return fmt.Errorf("exvol-attach: no action fn")
	}
	return a.Fn(ctx)
}

// Delete implements reconcile.Deleter: unmount (lazy fallback after
// the timeout), per §4.7. Closing the device and releasing the primary
// lease are the volume runtime's job.
func (m *Manager) Delete(ctx context.Context, r reconcile.Resource) error {
	a := r.(*attachRes)
	host := HostPath(m.Base, a.spec.VolID)
	if _, err := m.Run.Run(ctx, "umount", host); err != nil {
		// Grace period, then the lazy fallback (§4.7).
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
		if _, lerr := m.Run.Run(ctx, "umount", "-l", host); lerr != nil {
			return fmt.Errorf("umount %s: %v / %v", host, err, lerr)
		}
	}
	return nil
}

// attach waits for the device, formats only if blank, mounts noatime.
func (m *Manager) attach(ctx context.Context, spec Resource) error {
	dev := "/dev/exvol/" + spec.VolID

	// §4.7 step 3: wait up to 30 s for /dev/exvol/<id> (the volume
	// runtime attaches the device when the node is primary).
	deadline := time.Now().Add(m.Wait)
	for {
		if _, err := m.Run.Run(ctx, "test", "-e", dev); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("device %s did not appear within %v", dev, m.Wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}

	// §4.7 step 4 — THE HARD RULE: never reformat an existing
	// filesystem. blkid decides; an existing fs is left alone.
	out, err := m.Run.Run(ctx, "blkid", "-o", "value", "-s", "TYPE", dev)
	existing := strings.TrimSpace(out)
	if err != nil && existing == "" && !blkidClean(err) {
		return fmt.Errorf("blkid %s: %v", dev, err)
	}
	if existing == "" && spec.Filesystem != "" && spec.Filesystem != "none" {
		if _, err := m.Run.Run(ctx, "mkfs."+spec.Filesystem, "-F", dev); err != nil {
			return fmt.Errorf("mkfs.%s %s: %v", spec.Filesystem, dev, err)
		}
	}

	// §4.7 step 5: mount at the host path with noatime.
	host := HostPath(m.Base, spec.VolID)
	if _, err := m.Run.Run(ctx, "mkdir", "-p", host); err != nil {
		return fmt.Errorf("mkdir %s: %v", host, err)
	}
	if _, err := m.Run.Run(ctx, "findmnt", "-rn", "-S", dev, "-n", "-o", "TARGET"); err != nil {
		if _, err := m.Run.Run(ctx, "mount", "-o", "noatime", dev, host); err != nil {
			return fmt.Errorf("mount %s at %s: %v", dev, host, err)
		}
	}
	return nil
}

func blkidClean(err error) bool { return err != nil && strings.Contains(err.Error(), "exit status 2") }
