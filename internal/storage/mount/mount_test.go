package mount

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/reconcile"
	"github.com/expanse/expanse/internal/storage/drbd"
)

const (
	volID = "vol-a"
	dev   = "/dev/drbd7"
)

// fakeHost plays the parts of the machine the manager shells out to.
type fakeHost struct {
	calls    []string
	fsType   map[string]string // device → blkid TYPE
	blank    map[string]bool   // device → first MiB is all zero
	unread   bool              // cmp cannot read the device
	mounted  map[string]string // mount point → device
	busy     bool              // a plain umount fails
	devBytes uint64
	fsBytes  uint64
}

func newHost() *fakeHost {
	return &fakeHost{
		fsType:   map[string]string{},
		blank:    map[string]bool{dev: true},
		mounted:  map[string]string{},
		devBytes: 1 << 30,
		fsBytes:  1 << 30,
	}
}

func exit(code int) error { return &ExitError{Code: code} }

func (h *fakeHost) Run(_ context.Context, name string, args ...string) (string, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	h.calls = append(h.calls, cmd)
	switch name {
	case "blkid":
		if t := h.fsType[args[len(args)-1]]; t != "" {
			return t + "\n", nil
		}
		return "", exit(2)
	case "cmp":
		switch {
		case h.unread:
			return "", exit(2)
		case h.blank[args[len(args)-1]]:
			return "", nil
		}
		return "differ", exit(1)
	case "mkfs.ext4":
		h.fsType[args[1]] = "ext4"
	case "findmnt":
		if d, ok := h.mounted[args[len(args)-3]]; ok {
			return d + "\n", nil
		}
		return "", exit(1)
	case "mount":
		h.mounted[args[3]] = args[2]
	case "umount":
		if args[0] == "-l" {
			delete(h.mounted, args[1])
		} else if h.busy {
			return "", exit(32)
		} else {
			delete(h.mounted, args[0])
		}
	case "blockdev":
		return fmt.Sprintf("%d\n", h.devBytes), nil
	case "dumpe2fs":
		return fmt.Sprintf("Block count:              %d\nBlock size:               4096\n", h.fsBytes/4096), nil
	case "resize2fs":
		h.fsBytes = h.devBytes
	}
	return "", nil
}

func (h *fakeHost) ran(prefix string) bool {
	for _, c := range h.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

type fakeDRBD struct {
	role drbd.Role
	err  error
}

func (f fakeDRBD) Status(context.Context, string) (*drbd.Status, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &drbd.Status{Role: f.role, Volumes: []drbd.Volume{{Minor: 7}}}, nil
}

func manager(h *fakeHost, role drbd.Role) *Manager {
	m := New(h, fakeDRBD{role: role}, "")
	m.Grace = time.Millisecond
	return m
}

func spec(fs string) Resource {
	return Resource{VolID: volID, MountPath: "/var/lib/db", Filesystem: fs}
}

func TestAttachFormatsABlankDeviceAndMountsTheDRBDDevice(t *testing.T) {
	h := newHost()
	if err := manager(h, drbd.RolePrimary).attach(context.Background(), spec("ext4")); err != nil {
		t.Fatal(err)
	}
	if !h.ran("mkfs.ext4 -F "+dev) || h.mounted[HostPath("", volID)] != dev {
		t.Fatalf("want a formatted %s mounted at %s, got calls %v", dev, HostPath("", volID), h.calls)
	}
	if !h.ran("mount -o noatime " + dev) {
		t.Fatalf("mount must be noatime: %v", h.calls)
	}
}

func TestAttachNeverFormatsAnExistingFilesystem(t *testing.T) {
	h := newHost()
	h.fsType[dev] = "ext4"
	h.blank[dev] = false
	if err := manager(h, drbd.RolePrimary).attach(context.Background(), spec("ext4")); err != nil {
		t.Fatal(err)
	}
	if h.ran("mkfs") || h.mounted[HostPath("", volID)] != dev {
		t.Fatalf("an existing filesystem must be mounted untouched: %v", h.calls)
	}
}

func TestAttachNeverFormatsDataItDoesNotRecognise(t *testing.T) {
	h := newHost()
	h.blank[dev] = false
	err := manager(h, drbd.RolePrimary).attach(context.Background(), spec("ext4"))
	if err == nil || h.ran("mkfs") || h.ran("mount") {
		t.Fatalf("unrecognised data must be refused, not formatted: err=%v calls=%v", err, h.calls)
	}
}

func TestAttachNeverFormatsADeviceItCannotRead(t *testing.T) {
	h := newHost()
	h.unread = true
	err := manager(h, drbd.RolePrimary).attach(context.Background(), spec("ext4"))
	if err == nil || h.ran("mkfs") {
		t.Fatalf("a read failure (quorum loss) must not read as blank: err=%v calls=%v", err, h.calls)
	}
}

func TestAttachLeavesARawDeviceAlone(t *testing.T) {
	for _, fs := range []string{"", "none"} {
		h := newHost()
		if err := manager(h, drbd.RolePrimary).attach(context.Background(), spec(fs)); err != nil {
			t.Fatal(err)
		}
		if h.ran("mkfs") {
			t.Errorf("filesystem %q must never be formatted", fs)
		}
	}
}

// TestAttachNeverMountsARawDevice is the regression test for
// PHASE-04-TASKS.md D3: a raw consumer (e.g. an iSCSI LUN backstore) is
// handed the bare device, never a directory mount — attempting `mount` on a
// device with no filesystem signature to autodetect would just fail in
// reality, a bug the fake host in this suite otherwise masks.
func TestAttachNeverMountsARawDevice(t *testing.T) {
	for _, fs := range []string{"", "none"} {
		h := newHost()
		if err := manager(h, drbd.RolePrimary).attach(context.Background(), spec(fs)); err != nil {
			t.Fatal(err)
		}
		if h.ran("mount") {
			t.Errorf("filesystem %q must never be mounted as a directory: %v", fs, h.calls)
		}
	}
}

// TestObserveIsInSyncForARawDeviceOnceItIsPrimary is the counterpart to
// TestObserveIsInSyncOnlyOnceMountedAtFullSize for a raw consumer: since
// nothing is ever mounted, "in sync" means the device resolves under this
// node's Primary role, not that findmnt shows a mount.
func TestObserveIsInSyncForARawDeviceOnceItIsPrimary(t *testing.T) {
	h := newHost()
	m := manager(h, drbd.RolePrimary)
	r, err := m.Load("volume-mount:"+volID, []byte(`{"volId":"vol-a","mountPath":"/x","filesystem":"none"}`))
	if err != nil {
		t.Fatal(err)
	}
	o, err := m.Observe(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !o.InSync || o.Health != reconcile.HealthHealthy {
		t.Fatalf("want a raw device on the Primary node in sync, got %+v", o)
	}
	if h.ran("mount") || h.ran("findmnt") {
		t.Fatalf("observing a raw device must never attempt a mount check: %v", h.calls)
	}
}

// Opening a DRBD device on a Secondary would promote it behind the lease's back.
func TestAttachTouchesNothingUnlessThisNodeIsPrimary(t *testing.T) {
	for _, role := range []drbd.Role{drbd.RoleSecondary, drbd.RoleUnknown} {
		h := newHost()
		err := manager(h, role).attach(context.Background(), spec("ext4"))
		if err == nil || experrors.KindOf(err) != experrors.KindConflict {
			t.Fatalf("role %q: want a Conflict, got %v", role, err)
		}
		if len(h.calls) != 0 {
			t.Fatalf("role %q: the device was touched: %v", role, h.calls)
		}
	}
}

func TestAttachFailsForAVolumeDRBDDoesNotKnow(t *testing.T) {
	h := newHost()
	m := New(h, fakeDRBD{err: experrors.New(experrors.KindNotFound, "t", "no such resource")}, "")
	if err := m.attach(context.Background(), spec("ext4")); err == nil || len(h.calls) != 0 {
		t.Fatalf("want an error and no commands, got %v / %v", err, h.calls)
	}
}

func TestAttachIsRepeatable(t *testing.T) {
	h := newHost()
	m := manager(h, drbd.RolePrimary)
	for i := 0; i < 2; i++ {
		if err := m.attach(context.Background(), spec("ext4")); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(strings.Join(h.calls, "\n"), "mount -o"); n != 1 {
		t.Fatalf("mounted %d times, want once: %v", n, h.calls)
	}
}

// A fresh mount's root must be writable by a DynamicUser workload
// (PHASE-05-TASKS.md Stream A: db/postgres, the first storage-bound
// block that isn't RunAsRoot) whose uid systemd only allocates once its
// unit starts — unknowable here to chown to in advance, so world-
// writable is the only workable fix (regression test for the
// "permission denied" mkdir a DynamicUser workload hit against a
// freshly mounted, root:root 0755 filesystem).
func TestAttachMakesAFreshMountWorldWritable(t *testing.T) {
	h := newHost()
	if err := manager(h, drbd.RolePrimary).attach(context.Background(), spec("ext4")); err != nil {
		t.Fatal(err)
	}
	if !h.ran("chmod 0777 " + HostPath("", volID)) {
		t.Fatalf("want the fresh mount chmod'd 0777: %v", h.calls)
	}
}

// The chmod must run only on the mount that actually just happened, not
// every reconcile pass — a workload's own later permission changes
// inside the mount must never be stomped on by a routine re-attach.
func TestAttachOnlyChmodsOnTheInitialMount(t *testing.T) {
	h := newHost()
	m := manager(h, drbd.RolePrimary)
	for i := 0; i < 2; i++ {
		if err := m.attach(context.Background(), spec("ext4")); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(strings.Join(h.calls, "\n"), "chmod 0777"); n != 1 {
		t.Fatalf("chmod'd %d times, want once: %v", n, h.calls)
	}
}

func observe(t *testing.T, m *Manager) reconcile.Observed {
	t.Helper()
	r, err := m.Load("volume-mount:"+volID, []byte(`{"volId":"vol-a","mountPath":"/x","filesystem":"ext4"}`))
	if err != nil {
		t.Fatal(err)
	}
	o, err := m.Observe(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestObserveIsInSyncOnlyOnceMountedAtFullSize(t *testing.T) {
	h := newHost()
	m := manager(h, drbd.RolePrimary)
	if observe(t, m).InSync {
		t.Fatal("an unmounted volume is not in sync")
	}
	if err := m.attach(context.Background(), spec("ext4")); err != nil {
		t.Fatal(err)
	}
	if !observe(t, m).InSync {
		t.Fatal("a mounted, full-size volume is in sync")
	}
	h.devBytes = 2 << 30
	if observe(t, m).InSync {
		t.Fatal("a device larger than its filesystem is not in sync")
	}
}

func TestPlanGrowsAMountedFilesystemAfterAResize(t *testing.T) {
	h := newHost()
	m := manager(h, drbd.RolePrimary)
	if err := m.attach(context.Background(), spec("ext4")); err != nil {
		t.Fatal(err)
	}
	h.devBytes = 2 << 30
	r, _ := m.Load("volume-mount:"+volID, []byte(`{"volId":"vol-a","mountPath":"/x","filesystem":"ext4"}`))
	acts, err := m.Plan(context.Background(), r, observe(t, m))
	if err != nil || len(acts) != 1 {
		t.Fatalf("want one action, got %v / %v", acts, err)
	}
	if h.ran("umount") {
		t.Fatal("growing must be online")
	}
	if err := m.Apply(context.Background(), acts[0]); err != nil {
		t.Fatal(err)
	}
	if !h.ran("resize2fs "+dev) || !observe(t, m).InSync {
		t.Fatalf("want the filesystem grown to the device: %v", h.calls)
	}
}

func TestReleaseUnmountsAndIsRepeatable(t *testing.T) {
	h := newHost()
	m := manager(h, drbd.RolePrimary)
	if err := m.attach(context.Background(), spec("ext4")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := m.Release(context.Background(), volID); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.mounted) != 0 {
		t.Fatalf("still mounted: %v", h.mounted)
	}
}

func TestReleaseOfAnUnmountedVolumeRunsNoUnmount(t *testing.T) {
	h := newHost()
	if err := manager(h, drbd.RolePrimary).Release(context.Background(), volID); err != nil {
		t.Fatal(err)
	}
	if h.ran("umount") {
		t.Fatalf("nothing was mounted: %v", h.calls)
	}
}

func TestReleaseFallsBackToALazyUnmountWhenBusy(t *testing.T) {
	h := newHost()
	h.busy = true
	m := manager(h, drbd.RolePrimary)
	if err := m.attach(context.Background(), spec("ext4")); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(context.Background(), volID); err != nil {
		t.Fatal(err)
	}
	if !h.ran("umount -l "+HostPath("", volID)) || len(h.mounted) != 0 {
		t.Fatalf("lazy fallback not used: %v", h.calls)
	}
}

func TestReleaseStopsWaitingWhenCancelled(t *testing.T) {
	h := newHost()
	h.busy = true
	m := manager(h, drbd.RolePrimary)
	m.Grace = time.Minute
	if err := m.attach(context.Background(), spec("ext4")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Release(ctx, volID); err == nil {
		t.Fatal("a cancelled release must report failure")
	}
}

func TestDeleteUnmountsTheVolume(t *testing.T) {
	h := newHost()
	m := manager(h, drbd.RolePrimary)
	if err := m.attach(context.Background(), spec("ext4")); err != nil {
		t.Fatal(err)
	}
	r, _ := m.Load("volume-mount:"+volID, []byte(`{"volId":"vol-a","mountPath":"/x"}`))
	if err := m.Delete(context.Background(), r); err != nil || len(h.mounted) != 0 {
		t.Fatalf("delete left %v mounted (err %v)", h.mounted, err)
	}
}

func TestLoadValidatesTheRequiredFields(t *testing.T) {
	m := manager(newHost(), drbd.RolePrimary)
	if _, err := m.Load("x", []byte(`{"mountPath":"/x"}`)); err == nil {
		t.Fatal("volId required")
	}
	if _, err := m.Load("x", []byte(`{"volId":"v"}`)); err == nil {
		t.Fatal("mountPath required")
	}
}
