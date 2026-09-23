package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/storage/drbd"
)

// mountPaths/firstMount decode the bridge's "--mount name=path" args
// (internal/blocks/wire/bridge.go replicaSpec) — the only channel a
// storage-bound workload (share/smb, share/nfs) has for its own
// mountPath, since --config carries only the user's schema-validated
// spec.config.
func TestMountPathsParsesNamePathPairs(t *testing.T) {
	args := []string{"--config", `{"port":445}`, "--mount", "share-data=/mnt/share-data"}
	got := mountPaths(args)
	if got["share-data"] != "/mnt/share-data" {
		t.Errorf("mountPaths(%v) = %v", args, got)
	}
}

func TestMountPathsEmptyWithNoMountArg(t *testing.T) {
	if got := mountPaths([]string{"--config", "{}"}); len(got) != 0 {
		t.Errorf("mountPaths = %v, want empty", got)
	}
}

func TestFirstMountIsDeterministic(t *testing.T) {
	got := firstMount(map[string]string{"z-data": "/mnt/z", "a-data": "/mnt/a"})
	if got != "/mnt/a" {
		t.Errorf("firstMount = %q, want the lowest-sorted key's path", got)
	}
}

func TestFirstMountEmptyWhenNoMount(t *testing.T) {
	if got := firstMount(map[string]string{}); got != "" {
		t.Errorf("firstMount = %q, want empty", got)
	}
}

// voldevs/firstVoldev decode the bridge's "--voldev name=volID" args
// (bridge.go's raw-entry branch, PHASE-04-TASKS.md D3) — the channel a
// raw-storage workload (iscsi/target) has for its bound volume's ID,
// since a raw entry has no host mount path to hand it instead.
func TestVoldevsParsesNameIDPairs(t *testing.T) {
	args := []string{"--config", `{"port":3260}`, "--voldev", "lun=vol-1"}
	got := voldevs(args)
	if got["lun"] != "vol-1" {
		t.Errorf("voldevs(%v) = %v", args, got)
	}
}

func TestVoldevsEmptyWithNoVoldevArg(t *testing.T) {
	if got := voldevs([]string{"--config", "{}"}); len(got) != 0 {
		t.Errorf("voldevs = %v, want empty", got)
	}
}

func TestFirstVoldevIsDeterministic(t *testing.T) {
	got := firstVoldev(map[string]string{"z-lun": "vol-z", "a-lun": "vol-a"})
	if got != "vol-a" {
		t.Errorf("firstVoldev = %q, want the lowest-sorted key's volume ID", got)
	}
}

func TestFirstVoldevEmptyWhenNoVoldev(t *testing.T) {
	if got := firstVoldev(map[string]string{}); got != "" {
		t.Errorf("firstVoldev = %q, want empty", got)
	}
}

// waitForMount must never let a storage-bound workload proceed against
// a path that isn't yet a real, distinct mounted filesystem: the
// volume-mount reconcile resource (internal/storage/mount) converges
// independently of, and can lag behind, the block-replica spec that
// names mountPath (bridge.go) — writing to the path too early lands on
// the pre-mount directory, which the real mount then shadows once it
// lands, hiding everything just written (reproduced directly against
// smbd, which found its own state directory missing this way).
func TestWaitForMountSucceedsOnceDeviceDiffers(t *testing.T) {
	calls := 0
	stat := func(path string, st *syscall.Stat_t) error {
		if path == "/parent" {
			st.Dev = 1
			return nil
		}
		calls++
		// Same device (not yet mounted) for the first two polls, then a
		// distinct device (the real filesystem has landed).
		if calls < 3 {
			st.Dev = 1
		} else {
			st.Dev = 2
		}
		return nil
	}
	if err := waitForMount("/parent/mnt", time.Second, time.Millisecond, stat); err != nil {
		t.Fatalf("waitForMount: %v", err)
	}
	if calls < 3 {
		t.Errorf("returned before the device actually differed: %d calls", calls)
	}
}

func TestWaitForMountTimesOutWhenNeverDistinct(t *testing.T) {
	stat := func(path string, st *syscall.Stat_t) error {
		st.Dev = 1 // parent and path always the same device
		return nil
	}
	err := waitForMount("/parent/mnt", 20*time.Millisecond, time.Millisecond, stat)
	if err == nil {
		t.Fatal("want a timeout error, got nil")
	}
}

func TestWaitForMountRetriesThroughStatErrors(t *testing.T) {
	calls := 0
	stat := func(path string, st *syscall.Stat_t) error {
		calls++
		if calls < 3 {
			return errors.New("no such file or directory") // not created yet
		}
		st.Dev = map[string]uint64{"/parent": 1, "/parent/mnt": 2}[path]
		return nil
	}
	if err := waitForMount("/parent/mnt", time.Second, time.Millisecond, stat); err != nil {
		t.Fatalf("waitForMount: %v", err)
	}
}

// fakeDRBDStatuser is an in-memory drbdStatuser for waitForPrimaryDevice's
// tests: a queue of canned responses, one per call.
type fakeDRBDStatuser struct {
	responses []func() (*drbd.Status, error)
	calls     int
}

func (f *fakeDRBDStatuser) Status(context.Context, string) (*drbd.Status, error) {
	i := f.calls
	if i >= len(f.responses) {
		i = len(f.responses) - 1
	}
	f.calls++
	return f.responses[i]()
}

// waitForPrimaryDevice must never let iscsi/target proceed against a
// volume that isn't yet Primary on this node (D1, revised: SINGLETON
// only ever places the replica on the DRBD-primary node, but the mount
// resource and the block-replica spec still converge independently, the
// same race waitForMount guards against for a filesystem-backed entry).
func TestWaitForPrimaryDeviceSucceedsOncePrimaryWithAVolume(t *testing.T) {
	fake := &fakeDRBDStatuser{responses: []func() (*drbd.Status, error){
		func() (*drbd.Status, error) { return &drbd.Status{Role: drbd.RoleSecondary}, nil },
		func() (*drbd.Status, error) {
			return &drbd.Status{Role: drbd.RolePrimary, Volumes: []drbd.Volume{{Minor: 7}}}, nil
		},
	}}
	dev, err := waitForPrimaryDevice(context.Background(), fake, "vol-1", time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("waitForPrimaryDevice: %v", err)
	}
	if dev != "/dev/drbd7" {
		t.Errorf("waitForPrimaryDevice = %q, want /dev/drbd7", dev)
	}
	if fake.calls < 2 {
		t.Errorf("returned before the role actually became Primary: %d calls", fake.calls)
	}
}

func TestWaitForPrimaryDeviceTimesOutWhileSecondary(t *testing.T) {
	fake := &fakeDRBDStatuser{responses: []func() (*drbd.Status, error){
		func() (*drbd.Status, error) { return &drbd.Status{Role: drbd.RoleSecondary}, nil },
	}}
	_, err := waitForPrimaryDevice(context.Background(), fake, "vol-1", 20*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("want a timeout error, got nil")
	}
}

func TestWaitForPrimaryDeviceRetriesThroughStatusErrors(t *testing.T) {
	fake := &fakeDRBDStatuser{responses: []func() (*drbd.Status, error){
		func() (*drbd.Status, error) { return nil, errors.New("resource not configured yet") },
		func() (*drbd.Status, error) {
			return &drbd.Status{Role: drbd.RolePrimary, Volumes: []drbd.Volume{{Minor: 0}}}, nil
		},
	}}
	dev, err := waitForPrimaryDevice(context.Background(), fake, "vol-1", time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("waitForPrimaryDevice: %v", err)
	}
	if dev != "/dev/drbd0" {
		t.Errorf("waitForPrimaryDevice = %q, want /dev/drbd0", dev)
	}
}

// resetSMBEphemeralState must actually discard prior contents (a stale
// PID-keyed lock/session record from a crashed node's smbd, replicated
// onto this one via the shared volume, must not survive) while leaving
// the directory itself present and writable for the new smbd to use.
func TestResetSMBEphemeralStateDiscardsPriorContents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lock")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("setup mkdir: %v", err)
	}
	stale := filepath.Join(dir, "locking.tdb")
	if err := os.WriteFile(stale, []byte("stale pid-keyed record"), 0o640); err != nil {
		t.Fatalf("setup write: %v", err)
	}

	if err := resetSMBEphemeralState(dir); err != nil {
		t.Fatalf("resetSMBEphemeralState: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("locking.tdb: want removed, stat err = %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("dir: want a fresh, present directory, got info=%v err=%v", info, err)
	}
}

func TestResetSMBEphemeralStateCreatesMissingDirs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does", "not", "exist", "yet")
	if err := resetSMBEphemeralState(dir); err != nil {
		t.Fatalf("resetSMBEphemeralState: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("dir: want created, got info=%v err=%v", info, err)
	}
}

// defaultIQN/defaultWWN must be stable across every failover (D4): the
// same instance name always derives the same identity, on any node,
// with nothing node-specific in the inputs.
func TestDefaultIQNIsDeterministic(t *testing.T) {
	a := defaultIQN("default-lun-0")
	b := defaultIQN("default-lun-0")
	if a != b {
		t.Errorf("defaultIQN not deterministic: %q vs %q", a, b)
	}
	if defaultIQN("default-other-0") == a {
		t.Error("defaultIQN identical for two different instances")
	}
}

func TestDefaultWWNIsDeterministicAndWellFormed(t *testing.T) {
	a := defaultWWN("default-lun-0")
	b := defaultWWN("default-lun-0")
	if a != b {
		t.Errorf("defaultWWN not deterministic: %q vs %q", a, b)
	}
	if defaultWWN("default-other-0") == a {
		t.Error("defaultWWN identical for two different instances")
	}
	// NAA format code 5 (IEEE Registered, 64-bit): "naa." + 16 hex
	// digits, the same shape LIO's own random default WWNs use.
	const prefix = "naa.5"
	if len(a) != len(prefix)+15 || a[:len(prefix)] != prefix {
		t.Errorf("defaultWWN = %q, want %s<15 hex digits>", a, prefix)
	}
}

func TestSanitizeIDReplacesUnsafeCharacters(t *testing.T) {
	got := sanitizeID("Default/LUN_0")
	if got != "default-lun-0" {
		t.Errorf("sanitizeID = %q, want default-lun-0", got)
	}
}
