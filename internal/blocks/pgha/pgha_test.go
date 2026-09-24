package pgha

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/expanse/expanse/internal/cluster/lease"
	"github.com/expanse/expanse/internal/store/boltstore"
)

func newTestStore(t *testing.T) *boltstore.Store {
	t.Helper()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "pgha.db"))
	if err != nil {
		t.Fatalf("boltstore.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func readRole(t *testing.T, mount string) string {
	t.Helper()
	b, err := os.ReadFile(RolePath(mount))
	if err != nil {
		t.Fatalf("read role file: %v", err)
	}
	return string(b)
}

func TestElectSinglePrimary(t *testing.T) {
	st := newTestStore(t)
	c := New(Config{
		Leases:      lease.NewManager(st, "n1"),
		Self:        "n1",
		ResolveAddr: func(string) string { return "" },
	})
	inst := Instance{BlockRef: "default/pg", MountPath: t.TempDir(), Port: 5432}
	c.Pass(context.Background(), map[string]Instance{inst.BlockRef: inst})

	if got := readRole(t, inst.MountPath); got != "primary\n" {
		t.Errorf("role file = %q, want %q", got, "primary\n")
	}
	c.mu.Lock()
	_, held := c.active[inst.BlockRef]
	c.mu.Unlock()
	if !held {
		t.Error("winner did not keep the lease in active")
	}
}

// TestWriteRoleFileIsReadableAndWritableByADifferentUid is the
// regression test for PHASE-05-TASKS.md Stream A X1: this controller
// runs as root, but the db/postgres workload reading the role file (and
// creating its own subdirectories next to it) runs under a fixed,
// different, non-root uid. The VM test found the previous 0700/0600
// modes locked the workload out entirely once the controller created
// the directory first.
func TestWriteRoleFileIsReadableAndWritableByADifferentUid(t *testing.T) {
	st := newTestStore(t)
	c := New(Config{
		Leases:      lease.NewManager(st, "n1"),
		Self:        "n1",
		ResolveAddr: func(string) string { return "" },
	})
	mount := t.TempDir()
	inst := Instance{BlockRef: "default/pg", MountPath: mount, Port: 5432}
	c.Pass(context.Background(), map[string]Instance{inst.BlockRef: inst})

	dirInfo, err := os.Stat(filepath.Dir(RolePath(mount)))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm&0o022 == 0 {
		t.Errorf("state dir mode = %v, want group/other write so a different uid can create its own files there", perm)
	}
	fileInfo, err := os.Stat(RolePath(mount))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fileInfo.Mode().Perm(); perm&0o044 == 0 {
		t.Errorf("role file mode = %v, want group/other read so a different uid can read the decision", perm)
	}
}

func TestElectReplicaFollowsExistingPrimary(t *testing.T) {
	st := newTestStore(t)
	ref := "default/pg"
	// n1 already won election in a previous pass.
	leaseMgr := lease.NewManager(st, "n1")
	held, err := leaseMgr.TryAcquire(context.Background(), LeaseName(ref), LeaseTTL)
	if err != nil {
		t.Fatalf("seed primary lease: %v", err)
	}
	defer held.Abandon()

	c := New(Config{
		Leases: lease.NewManager(st, "n2"),
		Self:   "n2",
		ResolveAddr: func(node string) string {
			if node == "n1" {
				return "10.42.0.5"
			}
			return ""
		},
	})
	inst := Instance{BlockRef: ref, MountPath: t.TempDir(), Port: 5432}
	c.Pass(context.Background(), map[string]Instance{ref: inst})

	want := "replica 10.42.0.5 5432\n"
	if got := readRole(t, inst.MountPath); got != want {
		t.Errorf("role file = %q, want %q", got, want)
	}
	c.mu.Lock()
	_, held2 := c.active[ref]
	c.mu.Unlock()
	if held2 {
		t.Error("a replica must not hold the primary lease")
	}
}

func TestElectReplicaWaitsForUnresolvedAddr(t *testing.T) {
	st := newTestStore(t)
	ref := "default/pg"
	leaseMgr := lease.NewManager(st, "n1")
	held, err := leaseMgr.TryAcquire(context.Background(), LeaseName(ref), LeaseTTL)
	if err != nil {
		t.Fatalf("seed primary lease: %v", err)
	}
	defer held.Abandon()

	c := New(Config{
		Leases:      lease.NewManager(st, "n2"),
		Self:        "n2",
		ResolveAddr: func(string) string { return "" }, // address not yet known
	})
	mount := t.TempDir()
	inst := Instance{BlockRef: ref, MountPath: mount, Port: 5432}
	c.Pass(context.Background(), map[string]Instance{ref: inst})

	if _, err := os.ReadFile(RolePath(mount)); !os.IsNotExist(err) {
		t.Errorf("role file written despite unresolved primary address: err=%v", err)
	}
}

func TestOnDecidedPrimaryReclaimsAcrossRestart(t *testing.T) {
	st := newTestStore(t)
	ref := "default/pg"
	mount := t.TempDir()
	if err := writeRoleFile(RolePath(mount), primaryContent); err != nil {
		t.Fatalf("seed role file: %v", err)
	}

	// Simulate a live self-held record from "before the restart":
	// TryAcquire it once, then abandon the Held object (drop it) without
	// releasing the store record — that's exactly what a hard crash
	// leaves behind.
	seed := lease.NewManager(st, "n1")
	held, err := seed.TryAcquire(context.Background(), LeaseName(ref), LeaseTTL)
	if err != nil {
		t.Fatalf("seed lease: %v", err)
	}
	held.Abandon()

	c := New(Config{
		Leases:      lease.NewManager(st, "n1"),
		Self:        "n1",
		ResolveAddr: func(string) string { return "" },
	})
	inst := Instance{BlockRef: ref, MountPath: mount, Port: 5432}
	c.Pass(context.Background(), map[string]Instance{ref: inst})

	c.mu.Lock()
	_, reclaimed := c.active[ref]
	c.mu.Unlock()
	if !reclaimed {
		t.Error("restarted primary did not reclaim its lease")
	}
	// The role file is never rewritten on reclaim.
	if got := readRole(t, mount); got != primaryContent {
		t.Errorf("role file changed on reclaim: %q", got)
	}
}

func TestDecidedReplicaIsNoop(t *testing.T) {
	st := newTestStore(t)
	ref := "default/pg"
	mount := t.TempDir()
	if err := writeRoleFile(RolePath(mount), "replica 10.42.0.5 5432\n"); err != nil {
		t.Fatalf("seed role file: %v", err)
	}
	c := New(Config{
		Leases:      lease.NewManager(st, "n2"),
		Self:        "n2",
		ResolveAddr: func(string) string { return "" },
	})
	inst := Instance{BlockRef: ref, MountPath: mount, Port: 5432}
	c.Pass(context.Background(), map[string]Instance{ref: inst})

	c.mu.Lock()
	_, held := c.active[ref]
	c.mu.Unlock()
	if held {
		t.Error("a decided replica must never attempt or hold the primary lease")
	}
}

func TestPassReleasesGoneInstance(t *testing.T) {
	st := newTestStore(t)
	c := New(Config{
		Leases:      lease.NewManager(st, "n1"),
		Self:        "n1",
		ResolveAddr: func(string) string { return "" },
	})
	ref := "default/pg"
	inst := Instance{BlockRef: ref, MountPath: t.TempDir(), Port: 5432}
	c.Pass(context.Background(), map[string]Instance{ref: inst})

	c.mu.Lock()
	_, held := c.active[ref]
	c.mu.Unlock()
	if !held {
		t.Fatal("setup: expected election to succeed")
	}

	c.Pass(context.Background(), map[string]Instance{}) // instance gone

	c.mu.Lock()
	_, stillHeld := c.active[ref]
	c.mu.Unlock()
	if stillHeld {
		t.Error("lease not released when instance disappeared")
	}
	l, ok, err := lease.NewManager(st, "n1").Inspect(context.Background(), LeaseName(ref))
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if ok {
		t.Errorf("lease record not removed on release: %+v", l)
	}
}

func TestStopReleasesAll(t *testing.T) {
	st := newTestStore(t)
	c := New(Config{
		Leases:      lease.NewManager(st, "n1"),
		Self:        "n1",
		ResolveAddr: func(string) string { return "" },
	})
	ref := "default/pg"
	inst := Instance{BlockRef: ref, MountPath: t.TempDir(), Port: 5432}
	c.Pass(context.Background(), map[string]Instance{ref: inst})
	c.Stop()

	c.mu.Lock()
	n := len(c.active)
	c.mu.Unlock()
	if n != 0 {
		t.Errorf("Stop left %d leases active", n)
	}
}

func TestLeaseNameAndRolePath(t *testing.T) {
	if got := LeaseName("default/pg"); got != "pg-primary:default/pg" {
		t.Errorf("LeaseName = %q", got)
	}
	if got, want := RolePath("/mnt/pg"), "/mnt/pg/.expanse-postgres/role"; got != want {
		t.Errorf("RolePath = %q, want %q", got, want)
	}
}
