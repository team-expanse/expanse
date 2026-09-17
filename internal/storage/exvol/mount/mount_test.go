package mount

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var fmu sync.Mutex

// fakeRunner records commands and answers the filesystem probes.
type fakeRunner struct {
	mu       sync.Mutex
	commands []string
	devices  map[string]string // device → exists
	filesys  map[string]string // device → fs TYPE (blkid)
	mounted  map[string]string // device → target
	failMKFS bool
}

func newRunner() *fakeRunner {
	return &fakeRunner{
		filesys: map[string]string{},
		mounted: map[string]string{},
	}
}

var ws = regexp.MustCompile(`\s+`)

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := ws.ReplaceAllString(name+" "+strings.Join(args, " "), " ")
	f.commands = append(f.commands, cmd)
	switch {
	case strings.HasPrefix(cmd, "test -e "):
		dev := strings.TrimPrefix(cmd, "test -e ")
		if _, ok := f.devices[dev]; !ok {
			return "", fmt.Errorf("exit status 1")
		}
		return "", nil
	case strings.HasPrefix(cmd, "blkid "):
		dev := strings.Fields(cmd)[5]
		if t, ok := f.filesys[dev]; ok {
			return t, nil
		}
		return "", fmt.Errorf("exit status 2") // blkid's "nothing found"
	case strings.HasPrefix(cmd, "mkfs."):
		dev := strings.Fields(cmd)[2]
		if f.failMKFS {
			return "", fmt.Errorf("mkfs boom")
		}
		fs := strings.TrimPrefix(strings.Fields(cmd)[0], "mkfs.")
		f.filesys[dev] = fs
		return "", nil
	case strings.HasPrefix(cmd, "mount "):
		parts := strings.Fields(cmd)
		dev, target := parts[3], parts[4]
		f.mounted[dev] = target
		return "", nil
	case strings.HasPrefix(cmd, "findmnt -rn -S"):
		dev := strings.Fields(cmd)[5]
		if t, ok := f.mounted[dev]; ok {
			return t, nil
		}
		return "", fmt.Errorf("exit status 1")
	case strings.HasPrefix(cmd, "umount"):
		var target string
		if strings.Contains(cmd, " -l") {
			target = strings.Fields(cmd)[2]
		} else {
			target = strings.Fields(cmd)[1]
		}
		for d, t := range f.mounted {
			if t == target {
				delete(f.mounted, d)
			}
		}
		if strings.Contains(cmd, " -l") {
			return "", nil
		}
		return "", fmt.Errorf("target is busy")
	case strings.HasPrefix(cmd, "mkdir"):
		return "", nil
	}
	return "", nil
}

// addDevice registers a device as present (race-safe for the wait test).
func (f *fakeRunner) addDevice(dev string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.devices == nil {
		f.devices = map[string]string{}
	}
	f.devices[dev] = "blk"
}

func (f *fakeRunner) has(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func spec(id, mp, fs string) Resource {
	return Resource{VolID: id, MountPath: mp, Filesystem: fs}
}

// TestAttachFormatsBlankDeviceOnly is §4.7 step 4's hard rule: a
// pre-existing filesystem is NEVER reformatted.
func TestAttachFormatsBlankDeviceOnly(t *testing.T) {
	ctx := context.Background()
	r := newRunner()
	m := New(r, "")
	m.Wait = 50 * time.Millisecond
	r.devices = map[string]string{"/dev/exvol/vol-a": "blk"}

	// Blank device → mkfs once, mounted noatime.
	if err := m.attach(ctx, spec("vol-a", "/var/lib/db", "ext4")); err != nil {
		t.Fatal(err)
	}
	if !r.has("mkfs.ext4 -F /dev/exvol/vol-a") {
		t.Fatal("blank device must be formatted")
	}
	if r.mounted["/dev/exvol/vol-a"] != HostPath("", "vol-a") {
		t.Fatalf("mounted at %q, want %q", r.mounted["/dev/exvol/vol-a"], HostPath("", "vol-a"))
	}

	// Pre-existing filesystem → NO mkfs command, still mounted.
	r2 := newRunner()
	m2 := New(r2, "")
	m2.Wait = 50 * time.Millisecond
	r2.devices = map[string]string{"/dev/exvol/vol-b": "blk"}
	r2.filesys["/dev/exvol/vol-b"] = "ext4" // fs exists
	if err := m2.attach(ctx, spec("vol-b", "/var/lib/db", "ext4")); err != nil {
		t.Fatal(err)
	}
	if r2.has("mkfs") {
		t.Fatal("NEVER reformat an existing filesystem — mkfs ran")
	}
	if r2.mounted["/dev/exvol/vol-b"] == "" {
		t.Fatal("existing-fs device must still be mounted")
	}

	// filesystem "none" (raw device) → never format.
	r3 := newRunner()
	m3 := New(r3, "")
	m3.Wait = 50 * time.Millisecond
	r3.devices = map[string]string{"/dev/exvol/vol-c": "blk"}
	if err := m3.attach(ctx, spec("vol-c", "/raw", "none")); err != nil {
		t.Fatal(err)
	}
	if r3.has("mkfs") {
		t.Fatal("filesystem none must never be formatted")
	}
}

// TestAttachWaitsForDevice: the device appears mid-wait (§4.7 step 3,
// 30 s bound — shortened here).
func TestAttachWaitsForDevice(t *testing.T) {
	ctx := context.Background()
	r := newRunner()
	m := New(r, "")
	m.Wait = 2 * time.Second
	r.devices = nil // device absent
	done := make(chan error, 1)
	go func() { done <- m.attach(ctx, spec("vol-d", "/x", "ext4")) }()
	time.Sleep(100 * time.Millisecond)
	r.addDevice("/dev/exvol/vol-d") // device appears
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("attach failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("attach did not return after the device appeared")
	}
	if !r.has("mount -o noatime /dev/exvol/vol-d") {
		t.Fatal("expected noatime mount")
	}
}

// TestAttachTimeout: a device that never appears fails within the
// bound (never hangs).
func TestAttachTimeout(t *testing.T) {
	r := newRunner()
	m := New(r, "")
	m.Wait = 200 * time.Millisecond
	err := m.attach(context.Background(), spec("vol-e", "/x", "ext4"))
	if err == nil || !strings.Contains(err.Error(), "did not appear") {
		t.Fatalf("want timeout error, got %v", err)
	}
}

// TestDetachLazyFallback: a busy mount unmounts with `umount -l`.
func TestDetachLazyFallback(t *testing.T) {
	r := newRunner()
	m := New(r, "")
	r.mounted["/dev/exvol/vol-f"] = HostPath("", "vol-f")
	res, err := m.Load("exvol-attach:vol-f", []byte(`{"volId":"vol-f","mountPath":"/x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(context.Background(), res); err != nil {
		t.Fatal(err)
	}
	if !r.has("umount -l " + HostPath("", "vol-f")) {
		t.Fatal("lazy fallback not used")
	}
	if _, ok := r.mounted["/dev/exvol/vol-f"]; ok {
		t.Fatal("mount still recorded after detach")
	}
}

// TestResourceRoundTrip: Load validates required fields.
func TestResourceRoundTrip(t *testing.T) {
	m := New(newRunner(), "")
	if _, err := m.Load("x", []byte(`{"mountPath":"/x"}`)); err == nil {
		t.Fatal("volId required")
	}
	if _, err := m.Load("x", []byte(`{"volId":"v"}`)); err == nil {
		t.Fatal("mountPath required")
	}
}
