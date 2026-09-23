package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
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
