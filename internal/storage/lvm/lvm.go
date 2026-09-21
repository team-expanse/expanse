// Package lvm wraps the lvm command for the volume engine's backing storage:
// volume groups, thin pools, thin and thick volumes, thin snapshots. It is the
// only package permitted to exec lvm; everything else uses the LVM interface.
package lvm

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	experrors "github.com/expanse/expanse/internal/errors"
)

var (
	// LVM names: letters, digits and + _ . -, not starting with - and never "." or "..".
	lvmName    = regexp.MustCompile(`^[A-Za-z0-9+_.][A-Za-z0-9+_.-]*$`)
	devicePath = regexp.MustCompile(`^/[A-Za-z0-9_./+-]+$`)
)

// LVM is the volume engine's view of the local volume manager. Sizes are bytes;
// LVM rounds them up to its extent size, so read the result back with Get.
type LVM interface {
	VG(ctx context.Context, vg string) (VG, error)
	CreateVG(ctx context.Context, vg string, pvs ...string) error
	ExtendVG(ctx context.Context, vg string, pvs ...string) error
	CreateThinPool(ctx context.Context, vg, pool string, size uint64) error
	CreateThin(ctx context.Context, vg, pool, name string, size uint64) error
	CreateThick(ctx context.Context, vg, name string, size uint64) error
	// Extend only grows: DRBD cannot shrink a device, so shrinking is never offered.
	Extend(ctx context.Context, vg, name string, size uint64) error
	Remove(ctx context.Context, vg, name string) error
	Snapshot(ctx context.Context, vg, origin, name string) error
	Activate(ctx context.Context, vg, name string) error
	Get(ctx context.Context, vg, name string) (LV, error)
	List(ctx context.Context, vg string) ([]LV, error)
}

// DevicePath is where the device-mapper node of an active LV appears.
func DevicePath(vg, name string) string { return "/dev/" + vg + "/" + name }

// Exec implements LVM by shelling out. runCmd is the injection point tests use.
type Exec struct {
	LvmPath string
	runCmd  func(ctx context.Context, name string, args ...string) *exec.Cmd
}

var _ LVM = (*Exec)(nil)

// New creates the default exec-backed driver.
func New() *Exec { return &Exec{LvmPath: "lvm"} }

func (e *Exec) cmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	if e.runCmd != nil {
		return e.runCmd(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}

// run executes `lvm <sub> <args>` and returns stdout; errors carry lvm's stderr.
func (e *Exec) run(ctx context.Context, sub string, args ...string) (string, error) {
	cmd := e.cmd(ctx, e.LvmPath, append([]string{sub}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", execErr(ctx, "lvm."+sub, err, stderr.String())
	}
	return stdout.String(), nil
}

func execErr(ctx context.Context, op string, err error, stderr string) error {
	if ctx.Err() != nil {
		return experrors.Wrap(ctx.Err(), experrors.KindTimeout, op, "canceled")
	}
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = err.Error()
	}
	return experrors.Wrap(err, classify(msg), op, msg)
}

func classify(stderr string) experrors.Kind {
	s := strings.ToLower(stderr)
	switch {
	case strings.Contains(s, "not found"), strings.Contains(s, "failed to find"):
		return experrors.KindNotFound
	case strings.Contains(s, "already exists"):
		return experrors.KindConflict
	case strings.Contains(s, "insufficient free space"):
		return experrors.KindResourceExhausted
	case strings.Contains(s, "not larger than existing size"):
		return experrors.KindInvalid // a shrink request; Extend only grows
	}
	return experrors.KindInternal
}

func invalid(op, format string, args ...any) error {
	return experrors.New(experrors.KindInvalid, op, fmt.Sprintf(format, args...))
}

func checkNames(op string, names ...string) error {
	for _, n := range names {
		if !lvmName.MatchString(n) || n == "." || n == ".." {
			return invalid(op, "invalid lvm name %q", n)
		}
	}
	return nil
}

func checkSize(op string, size uint64) error {
	if size == 0 {
		return invalid(op, "size must be positive")
	}
	return nil
}

func checkDevices(op string, pvs []string) error {
	if len(pvs) == 0 {
		return invalid(op, "no physical volumes given")
	}
	for _, pv := range pvs {
		if !devicePath.MatchString(pv) {
			return invalid(op, "%q is not an absolute device path", pv)
		}
	}
	return nil
}

func bytesArg(size uint64) string { return strconv.FormatUint(size, 10) + "b" }

func ref(vg, name string) string { return vg + "/" + name }

// VG returns the volume group's capacity.
func (e *Exec) VG(ctx context.Context, vg string) (VG, error) {
	if err := checkNames("lvm.VG", vg); err != nil {
		return VG{}, err
	}
	out, err := e.run(ctx, "vgs", reportArgs(vgFields, vg)...)
	if err != nil {
		return VG{}, err
	}
	return parseVG(out)
}

func reportArgs(fields, target string) []string {
	return []string{"--noheadings", "--units", "b", "--nosuffix", "--separator", sep, "--options", fields, target}
}

// CreateVG creates the group on the devices. LVM refuses devices that already
// carry a signature, which keeps a restored disk from being wiped.
func (e *Exec) CreateVG(ctx context.Context, vg string, pvs ...string) error {
	if err := checkNames("lvm.CreateVG", vg); err != nil {
		return err
	}
	if err := checkDevices("lvm.CreateVG", pvs); err != nil {
		return err
	}
	_, err := e.run(ctx, "vgcreate", append([]string{vg}, pvs...)...)
	return err
}

// ExtendVG adds devices to an existing group.
func (e *Exec) ExtendVG(ctx context.Context, vg string, pvs ...string) error {
	if err := checkNames("lvm.ExtendVG", vg); err != nil {
		return err
	}
	if err := checkDevices("lvm.ExtendVG", pvs); err != nil {
		return err
	}
	_, err := e.run(ctx, "vgextend", append([]string{vg}, pvs...)...)
	return err
}

// CreateThinPool creates a thin pool of the given data size.
func (e *Exec) CreateThinPool(ctx context.Context, vg, pool string, size uint64) error {
	const op = "lvm.CreateThinPool"
	if err := checkNames(op, vg, pool); err != nil {
		return err
	}
	if err := checkSize(op, size); err != nil {
		return err
	}
	_, err := e.run(ctx, "lvcreate", "--yes", "--type", "thin-pool", "--size", bytesArg(size), "--name", pool, vg)
	return err
}

// CreateThin creates a thin volume with the given virtual size in a pool.
func (e *Exec) CreateThin(ctx context.Context, vg, pool, name string, size uint64) error {
	const op = "lvm.CreateThin"
	if err := checkNames(op, vg, pool, name); err != nil {
		return err
	}
	if err := checkSize(op, size); err != nil {
		return err
	}
	_, err := e.run(ctx, "lvcreate", "--yes", "--type", "thin", "--thinpool", ref(vg, pool),
		"--virtualsize", bytesArg(size), "--name", name)
	return err
}

// CreateThick creates a fully allocated linear volume.
func (e *Exec) CreateThick(ctx context.Context, vg, name string, size uint64) error {
	const op = "lvm.CreateThick"
	if err := checkNames(op, vg, name); err != nil {
		return err
	}
	if err := checkSize(op, size); err != nil {
		return err
	}
	_, err := e.run(ctx, "lvcreate", "--yes", "--size", bytesArg(size), "--name", name, vg)
	return err
}

// Extend grows a volume or a thin pool to size. Growing to the current size is a no-op.
func (e *Exec) Extend(ctx context.Context, vg, name string, size uint64) error {
	const op = "lvm.Extend"
	if err := checkNames(op, vg, name); err != nil {
		return err
	}
	if err := checkSize(op, size); err != nil {
		return err
	}
	_, err := e.run(ctx, "lvextend", "--size", bytesArg(size), ref(vg, name))
	if err != nil && strings.Contains(err.Error(), "No size change") {
		return nil
	}
	return err
}

// Remove deletes a volume, snapshot or pool.
func (e *Exec) Remove(ctx context.Context, vg, name string) error {
	if err := checkNames("lvm.Remove", vg, name); err != nil {
		return err
	}
	_, err := e.run(ctx, "lvremove", "--force", ref(vg, name))
	return err
}

// Snapshot creates a thin snapshot of a thin volume. It starts inactive; call Activate to use it.
func (e *Exec) Snapshot(ctx context.Context, vg, origin, name string) error {
	if err := checkNames("lvm.Snapshot", vg, origin, name); err != nil {
		return err
	}
	_, err := e.run(ctx, "lvcreate", "--snapshot", "--name", name, ref(vg, origin))
	return err
}

// Activate brings a volume online, including thin snapshots, which LVM flags to skip activation.
func (e *Exec) Activate(ctx context.Context, vg, name string) error {
	if err := checkNames("lvm.Activate", vg, name); err != nil {
		return err
	}
	_, err := e.run(ctx, "lvchange", "--activate", "y", "--ignoreactivationskip", ref(vg, name))
	return err
}

// Get returns one volume.
func (e *Exec) Get(ctx context.Context, vg, name string) (LV, error) {
	const op = "lvm.Get"
	if err := checkNames(op, vg, name); err != nil {
		return LV{}, err
	}
	out, err := e.run(ctx, "lvs", reportArgs(lvFields, ref(vg, name))...)
	if err != nil {
		return LV{}, err
	}
	lvs, err := parseLVs(out)
	if err != nil {
		return LV{}, err
	}
	if len(lvs) != 1 {
		return LV{}, malformed(op, "want one volume for %s/%s, got %d", vg, name, len(lvs))
	}
	return lvs[0], nil
}

// List returns the group's visible volumes; LVM's hidden internals are omitted.
func (e *Exec) List(ctx context.Context, vg string) ([]LV, error) {
	if err := checkNames("lvm.List", vg); err != nil {
		return nil, err
	}
	out, err := e.run(ctx, "lvs", reportArgs(lvFields, vg)...)
	if err != nil {
		return nil, err
	}
	return parseLVs(out)
}
