// Package mount is the agent-side resource that mounts a DRBD volume on the node
// where it is Primary, and the volume.Consumer that unmounts it before a demotion.
//
// It never formats anything it cannot prove is blank: an existing filesystem is
// mounted untouched, and any other data, or an unreadable device, is refused.
// It never opens a device on a node that is not Primary, because DRBD would
// promote the node behind the lease's back.
package mount

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/reconcile"
	"github.com/expanse/expanse/internal/storage/drbd"
)

// Type is the reconcile resource type this manager handles.
const Type = "volume-mount"

// DefaultBase is the host mount root for volume devices.
const DefaultBase = "/var/lib/expanse/volumes"

// unmountGrace is how long a busy mount is given before it is detached lazily.
const unmountGrace = 5 * time.Second

// blankProbeBytes is how much of the device must read as zero before it may be formatted.
const blankProbeBytes = 1 << 20

// Resource is the desired state of one volume's local mount.
type Resource struct {
	VolID      string `json:"volId"`
	Name       string `json:"name,omitempty"`
	MountPath  string `json:"mountPath"`
	Filesystem string `json:"filesystem,omitempty"` // ext4 | xfs | none
}

// Runner runs a command and returns its combined output. A command that ran and
// exited non-zero must fail with an *ExitError.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExitError is a command that ran and exited non-zero.
type ExitError struct {
	Code int
	Out  string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("exit status %d: %s", e.Code, strings.TrimSpace(e.Out))
}

// ExecRunner shells out for real.
type ExecRunner struct{}

// Run implements Runner with os/exec.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	var x *exec.ExitError
	if errors.As(err, &x) {
		return string(out), &ExitError{Code: x.ExitCode(), Out: string(out)}
	}
	return string(out), err
}

func exitCode(err error) int {
	var x *ExitError
	if errors.As(err, &x) {
		return x.Code
	}
	return -1
}

// Volumes reports the local DRBD state of a volume; *drbd.Exec implements it.
type Volumes interface {
	Status(ctx context.Context, res string) (*drbd.Status, error)
}

// Manager converges volume-mount resources.
type Manager struct {
	Base  string // host mount root (default DefaultBase)
	Run   Runner
	DRBD  Volumes
	Grace time.Duration // wait before a busy unmount goes lazy
}

// New builds the manager; a nil runner shells out.
func New(runner Runner, dr Volumes, base string) *Manager {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Manager{Base: base, Run: runner, DRBD: dr, Grace: unmountGrace}
}

// HostPath is the canonical host mount point for a volume's device.
func HostPath(base, volID string) string {
	if base == "" {
		base = DefaultBase
	}
	return base + "/" + volID + "/mnt"
}

type mountRes struct {
	id   string
	spec Resource
}

func (r *mountRes) ID() string             { return r.id }
func (r *mountRes) Type() string           { return Type }
func (r *mountRes) Dependencies() []string { return nil }

// Type implements reconcile.Manager.
func (m *Manager) Type() string { return Type }

// Load implements reconcile.Manager.
func (m *Manager) Load(id string, spec []byte) (reconcile.Resource, error) {
	var r Resource
	if err := json.Unmarshal(spec, &r); err != nil {
		return nil, fmt.Errorf("%s %s: decode: %w", Type, id, err)
	}
	if r.VolID == "" {
		return nil, fmt.Errorf("%s %s: volId required", Type, id)
	}
	if r.MountPath == "" {
		return nil, fmt.Errorf("%s %s: mountPath required", Type, id)
	}
	return &mountRes{id: id, spec: r}, nil
}

// device is the volume's DRBD device, and only while this node is Primary.
func (m *Manager) device(ctx context.Context, volID string) (string, error) {
	st, err := m.DRBD.Status(ctx, volID)
	if err != nil {
		return "", err
	}
	if st.Role != drbd.RolePrimary {
		return "", experrors.New(experrors.KindConflict, "mount.device", "volume "+volID+" is not primary on this node")
	}
	if len(st.Volumes) == 0 {
		return "", experrors.New(experrors.KindInternal, "mount.device", "volume "+volID+" has no device")
	}
	return "/dev/drbd" + strconv.Itoa(st.Volumes[0].Minor), nil
}

// mountedFrom is the device mounted at host, or "" when nothing is.
func (m *Manager) mountedFrom(ctx context.Context, host string) (string, error) {
	out, err := m.Run.Run(ctx, "findmnt", "-rn", "-M", host, "-o", "SOURCE")
	switch {
	case err == nil:
		return strings.TrimSpace(out), nil
	case exitCode(err) == 1:
		return "", nil
	}
	return "", fmt.Errorf("findmnt %s: %w", host, err)
}

// isRaw reports whether fs means "bare device, no filesystem" (PHASE-04-TASKS.md
// D3) — empty is bundled with "none" since it is never a real caller's intent to
// mount an unlabeled device without knowing its type, only to leave it alone.
func isRaw(fs string) bool {
	return fs == "" || fs == "none"
}

// Observe implements reconcile.Manager: a filesystem-backed volume is in sync
// once mounted at the host path and the filesystem fills it; a raw volume
// (D3) is in sync as soon as it resolves under this node's Primary role,
// since nothing is ever mounted for it to check.
func (m *Manager) Observe(ctx context.Context, r reconcile.Resource) (reconcile.Observed, error) {
	spec := r.(*mountRes).spec
	o := reconcile.Observed{Exists: true, Health: reconcile.HealthDegraded, Details: map[string]string{}}
	dev, err := m.device(ctx, spec.VolID)
	if err != nil {
		o.Details["reason"] = err.Error()
		return o, nil
	}
	if isRaw(spec.Filesystem) {
		o.Health, o.InSync = reconcile.HealthHealthy, true
		return o, nil
	}
	src, err := m.mountedFrom(ctx, HostPath(m.Base, spec.VolID))
	if err != nil {
		return o, err
	}
	if src != dev {
		return o, nil
	}
	o.Health = reconcile.HealthHealthy
	o.InSync = true
	if grow, err := m.needsGrow(ctx, dev, spec.Filesystem); err == nil && grow {
		o.InSync = false
	}
	return o, nil
}

// Plan implements reconcile.Manager.
func (m *Manager) Plan(ctx context.Context, r reconcile.Resource, o reconcile.Observed) ([]reconcile.Action, error) {
	a := r.(*mountRes)
	if o.InSync {
		return nil, nil
	}
	if o.Health == reconcile.HealthHealthy {
		return []reconcile.Action{{
			ResourceID: a.id, Kind: "update",
			Description: fmt.Sprintf("grow the filesystem of %s to fill its device", a.spec.VolID),
			Fn:          func(ctx context.Context) error { return m.grow(ctx, a.spec) },
		}}, nil
	}
	return []reconcile.Action{{
		ResourceID: a.id, Kind: "update",
		Description: fmt.Sprintf("mount %s at %s (format only if blank, noatime)", a.spec.VolID, HostPath(m.Base, a.spec.VolID)),
		Fn:          func(ctx context.Context) error { return m.attach(ctx, a.spec) },
	}}, nil
}

// Apply implements reconcile.Manager.
func (m *Manager) Apply(ctx context.Context, a reconcile.Action) error {
	if a.Fn == nil {
		return fmt.Errorf("%s: no action fn", Type)
	}
	return a.Fn(ctx)
}

// Delete implements reconcile.Deleter.
func (m *Manager) Delete(ctx context.Context, r reconcile.Resource) error {
	return m.Release(ctx, r.(*mountRes).spec.VolID)
}

// Release unmounts a volume, and succeeds when it is not mounted. It is the
// volume.Consumer the promoter runs before every demotion.
func (m *Manager) Release(ctx context.Context, volID string) error {
	host := HostPath(m.Base, volID)
	src, err := m.mountedFrom(ctx, host)
	if err != nil || src == "" {
		return err
	}
	if _, err := m.Run.Run(ctx, "umount", host); err == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(m.Grace):
	}
	if _, err := m.Run.Run(ctx, "umount", "-l", host); err != nil {
		return fmt.Errorf("umount %s: %w", host, err)
	}
	return nil
}

func (m *Manager) attach(ctx context.Context, spec Resource) error {
	dev, err := m.device(ctx, spec.VolID)
	if err != nil {
		return err
	}
	// A raw consumer (D3) gets the bare device, never a directory mount:
	// `mount` without a filesystem type to autodetect would just fail
	// against a genuinely raw device, and there is nothing to format.
	if isRaw(spec.Filesystem) {
		return nil
	}
	if err := m.ensureFilesystem(ctx, dev, spec.Filesystem); err != nil {
		return err
	}
	host := HostPath(m.Base, spec.VolID)
	if _, err := m.Run.Run(ctx, "mkdir", "-p", host); err != nil {
		return fmt.Errorf("mkdir %s: %w", host, err)
	}
	src, err := m.mountedFrom(ctx, host)
	if err != nil {
		return err
	}
	if src == "" {
		if _, err := m.Run.Run(ctx, "mount", "-o", "noatime", dev, host); err != nil {
			return fmt.Errorf("mount %s at %s: %w", dev, host, err)
		}
	}
	return m.grow(ctx, spec)
}

// ensureFilesystem formats dev only when it carries no signature and reads as
// entirely zero. Anything else, including a device it cannot read, is left alone.
func (m *Manager) ensureFilesystem(ctx context.Context, dev, fs string) error {
	out, err := m.Run.Run(ctx, "blkid", "-o", "value", "-s", "TYPE", dev)
	if existing := strings.TrimSpace(out); existing != "" {
		return nil
	}
	if err != nil && exitCode(err) != 2 {
		return fmt.Errorf("blkid %s: %w", dev, err)
	}
	if isRaw(fs) {
		return nil
	}
	if _, err := m.Run.Run(ctx, "cmp", "-n", strconv.Itoa(blankProbeBytes), "/dev/zero", dev); err != nil {
		if exitCode(err) == 1 {
			return experrors.New(experrors.KindConflict, "mount.ensureFilesystem", "refusing to format "+dev+": it has no filesystem but is not blank")
		}
		return fmt.Errorf("cannot read %s to prove it blank: %w", dev, err)
	}
	if _, err := m.Run.Run(ctx, "mkfs."+fs, "-F", dev); err != nil {
		return fmt.Errorf("mkfs.%s %s: %w", fs, dev, err)
	}
	return nil
}

// needsGrow reports whether an ext4 filesystem is smaller than its device.
// Other filesystems are not grown.
func (m *Manager) needsGrow(ctx context.Context, dev, fs string) (bool, error) {
	if fs != "ext4" {
		return false, nil
	}
	out, err := m.Run.Run(ctx, "blockdev", "--getsize64", dev)
	if err != nil {
		return false, err
	}
	devBytes, err := strconv.ParseUint(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return false, fmt.Errorf("blockdev %s: %w", dev, err)
	}
	out, err = m.Run.Run(ctx, "dumpe2fs", "-h", dev)
	if err != nil {
		return false, err
	}
	blocks, size := dumpField(out, "Block count:"), dumpField(out, "Block size:")
	if blocks == 0 || size == 0 {
		return false, fmt.Errorf("dumpe2fs %s: no size in output", dev)
	}
	return devBytes >= blocks*size+size, nil
}

func dumpField(out, label string) uint64 {
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, label); ok {
			n, _ := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			return n
		}
	}
	return 0
}

func (m *Manager) grow(ctx context.Context, spec Resource) error {
	dev, err := m.device(ctx, spec.VolID)
	if err != nil {
		return err
	}
	need, err := m.needsGrow(ctx, dev, spec.Filesystem)
	if err != nil || !need {
		return err
	}
	if _, err := m.Run.Run(ctx, "resize2fs", dev); err != nil {
		return fmt.Errorf("resize2fs %s: %w", dev, err)
	}
	return nil
}
