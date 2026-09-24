package pgha

import (
	"context"
	"fmt"
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

// TestDecidedReplicaStaysPutWhilePrimaryLeaseIsHeld is the steady-state
// case: a replica whose role file already names a decision retries
// TryAcquire every pass (Stream B, X2), but while the primary is alive
// and renewing that is always a conflict — a no-op indistinguishable
// from the pre-Stream-B behavior from the outside.
func TestDecidedReplicaStaysPutWhilePrimaryLeaseIsHeld(t *testing.T) {
	st := newTestStore(t)
	ref := "default/pg"
	leaseMgr := lease.NewManager(st, "n1")
	held, err := leaseMgr.TryAcquire(context.Background(), LeaseName(ref), LeaseTTL)
	if err != nil {
		t.Fatalf("seed primary lease: %v", err)
	}
	defer held.Abandon()

	mount := t.TempDir()
	if err := writeRoleFile(RolePath(mount), "replica 10.42.0.5 5432\n"); err != nil {
		t.Fatalf("seed role file: %v", err)
	}
	promoted := false
	c := New(Config{
		Leases:      lease.NewManager(st, "n2"),
		Self:        "n2",
		ResolveAddr: func(string) string { return "" },
		Promote:     func(Instance) error { promoted = true; return nil },
	})
	inst := Instance{BlockRef: ref, MountPath: mount, Port: 5432}
	c.Pass(context.Background(), map[string]Instance{ref: inst})

	c.mu.Lock()
	_, active := c.active[ref]
	c.mu.Unlock()
	if active {
		t.Error("a replica must not hold the primary lease while it is held elsewhere")
	}
	if promoted {
		t.Error("Promote called although the primary lease is still live")
	}
	if got := readRole(t, mount); got != "replica 10.42.0.5 5432\n" {
		t.Errorf("role file changed while primary lease is held: %q", got)
	}
}

// TestReplicaRetargetsWhenThePrimaryMoves is the regression test for the
// bug found running the X2 VM test past promotion itself working: a
// decided replica whose own role file still names the ORIGINAL primary
// must retarget once a DIFFERENT node has since won the lease (a past
// promotion), or its streaming connection -- and, with
// synchronous_standby_names active, the new primary's every write --
// stays stuck forever.
func TestReplicaRetargetsWhenThePrimaryMoves(t *testing.T) {
	st := newTestStore(t)
	ref := "default/pg"
	mount := t.TempDir()
	if err := writeRoleFile(RolePath(mount), "replica 10.42.0.5 5432\n"); err != nil {
		t.Fatalf("seed role file: %v", err)
	}
	// n3 has since won the lease (a promotion this replica hasn't heard
	// about yet).
	held, err := lease.NewManager(st, "n3").TryAcquire(context.Background(), LeaseName(ref), LeaseTTL)
	if err != nil {
		t.Fatalf("seed primary lease: %v", err)
	}
	defer held.Abandon()

	var gotInst Instance
	var gotHost string
	calls := 0
	c := New(Config{
		Leases: lease.NewManager(st, "n2"),
		Self:   "n2",
		ResolveAddr: func(node string) string {
			if node == "n3" {
				return "10.42.0.9"
			}
			return ""
		},
		Reconfigure: func(inst Instance, newHost string) error {
			calls++
			gotInst, gotHost = inst, newHost
			return nil
		},
	})
	inst := Instance{BlockRef: ref, MountPath: mount, Port: 5432}
	c.Pass(context.Background(), map[string]Instance{ref: inst})

	if calls != 1 {
		t.Fatalf("Reconfigure called %d times, want 1", calls)
	}
	if gotHost != "10.42.0.9" {
		t.Errorf("Reconfigure newHost = %q, want %q", gotHost, "10.42.0.9")
	}
	if gotInst.BlockRef != ref {
		t.Errorf("Reconfigure called with instance %+v", gotInst)
	}
	want := "replica 10.42.0.9 5432\n"
	if got := readRole(t, mount); got != want {
		t.Errorf("role file = %q, want %q", got, want)
	}
}

// TestReplicaDoesNotRetargetWhenThePrimaryIsUnchanged is the steady-state
// counterpart: the same node still holds the lease and still resolves to
// the host already on record, so there is nothing to reconfigure.
func TestReplicaDoesNotRetargetWhenThePrimaryIsUnchanged(t *testing.T) {
	st := newTestStore(t)
	ref := "default/pg"
	mount := t.TempDir()
	if err := writeRoleFile(RolePath(mount), "replica 10.42.0.5 5432\n"); err != nil {
		t.Fatalf("seed role file: %v", err)
	}
	held, err := lease.NewManager(st, "n1").TryAcquire(context.Background(), LeaseName(ref), LeaseTTL)
	if err != nil {
		t.Fatalf("seed primary lease: %v", err)
	}
	defer held.Abandon()

	calls := 0
	c := New(Config{
		Leases: lease.NewManager(st, "n2"),
		Self:   "n2",
		ResolveAddr: func(node string) string {
			if node == "n1" {
				return "10.42.0.5"
			}
			return ""
		},
		Reconfigure: func(Instance, string) error { calls++; return nil },
	})
	inst := Instance{BlockRef: ref, MountPath: mount, Port: 5432}
	c.Pass(context.Background(), map[string]Instance{ref: inst})

	if calls != 0 {
		t.Errorf("Reconfigure called %d times, want 0 (primary unchanged)", calls)
	}
	if got := readRole(t, mount); got != "replica 10.42.0.5 5432\n" {
		t.Errorf("role file changed although the primary is unchanged: %q", got)
	}
}

// TestReplicaRetargetRetriesNextPassWhenReconfigureFails: a failed
// retarget attempt (the new primary not reachable yet, e.g.) must not
// corrupt the role file -- it stays exactly as it was, so the next
// pass's comparison still correctly detects "not yet retargeted".
func TestReplicaRetargetRetriesNextPassWhenReconfigureFails(t *testing.T) {
	st := newTestStore(t)
	ref := "default/pg"
	mount := t.TempDir()
	if err := writeRoleFile(RolePath(mount), "replica 10.42.0.5 5432\n"); err != nil {
		t.Fatalf("seed role file: %v", err)
	}
	held, err := lease.NewManager(st, "n3").TryAcquire(context.Background(), LeaseName(ref), LeaseTTL)
	if err != nil {
		t.Fatalf("seed primary lease: %v", err)
	}
	defer held.Abandon()

	c := New(Config{
		Leases:      lease.NewManager(st, "n2"),
		Self:        "n2",
		ResolveAddr: func(string) string { return "10.42.0.9" },
		Reconfigure: func(Instance, string) error { return fmt.Errorf("connection refused") },
	})
	inst := Instance{BlockRef: ref, MountPath: mount, Port: 5432}
	c.Pass(context.Background(), map[string]Instance{ref: inst})

	if got := readRole(t, mount); got != "replica 10.42.0.5 5432\n" {
		t.Errorf("role file changed despite a failed Reconfigure: %q", got)
	}
}

func TestSlotNameMatchesTheWorkloadsSideDerivation(t *testing.T) {
	// cmd/expanse-block-run's own TestPgSlotNameHasNoHyphens asserts the
	// identical result for the same replica identity ("default-pg-0"),
	// proven byte-for-byte here without importing across the main/
	// library boundary.
	if got := SlotName("default", "pg", 0); got != "expanse_default_pg_0" {
		t.Errorf("SlotName = %q, want expanse_default_pg_0", got)
	}
}

// TestDecidedReplicaPromotesWhenThePrimaryLeaseIsFree is Stream B's core
// case: the primary's lease has expired (it died), and this replica
// wins the race to take it over — it must promote its own already-
// streaming postgres, not just relabel the role file.
func TestDecidedReplicaPromotesWhenThePrimaryLeaseIsFree(t *testing.T) {
	st := newTestStore(t)
	ref := "default/pg"
	mount := t.TempDir()
	if err := writeRoleFile(RolePath(mount), "replica 10.42.0.5 5432\n"); err != nil {
		t.Fatalf("seed role file: %v", err)
	}
	var promotedInst Instance
	promotions := 0
	c := New(Config{
		Leases:      lease.NewManager(st, "n2"),
		Self:        "n2",
		ResolveAddr: func(string) string { return "" },
		Promote: func(inst Instance) error {
			promotions++
			promotedInst = inst
			return nil
		},
	})
	inst := Instance{BlockRef: ref, MountPath: mount, Port: 5432}
	c.Pass(context.Background(), map[string]Instance{ref: inst})

	if promotions != 1 {
		t.Fatalf("Promote called %d times, want 1", promotions)
	}
	if promotedInst.BlockRef != ref {
		t.Errorf("Promote called with instance %+v, want BlockRef %q", promotedInst, ref)
	}
	c.mu.Lock()
	_, active := c.active[ref]
	c.mu.Unlock()
	if !active {
		t.Error("winner did not keep the primary lease after promoting")
	}
	if got := readRole(t, mount); got != primaryContent {
		t.Errorf("role file = %q, want %q", got, primaryContent)
	}
}

// TestPromotionAbandonsTheLeaseWhenPromoteFails: pg_promote() actually
// failing (e.g. the local socket isn't reachable yet) must not leave
// this node fenced as a primary that never promoted anything — another
// replica needs a chance once the abandoned lease expires.
func TestPromotionAbandonsTheLeaseWhenPromoteFails(t *testing.T) {
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
		Promote:     func(Instance) error { return fmt.Errorf("connection refused") },
	})
	inst := Instance{BlockRef: ref, MountPath: mount, Port: 5432}
	c.Pass(context.Background(), map[string]Instance{ref: inst})

	c.mu.Lock()
	_, active := c.active[ref]
	c.mu.Unlock()
	if active {
		t.Error("a failed promotion must not keep the lease active")
	}
	if got := readRole(t, mount); got != "replica 10.42.0.5 5432\n" {
		t.Errorf("role file changed despite a failed promotion: %q", got)
	}
}

// TestPromotionKeepsTheLeaseWhenTheRoleFileWriteFails is the D5
// (split-brain) invariant: once pg_promote() has actually succeeded,
// this node genuinely is primary, so a role-file write failure right
// afterward must never abandon the lease — doing so could let a second
// replica win and promote too. mount is a plain file, not a directory,
// so MkdirAll for the role file's own parent is guaranteed to fail.
func TestPromotionKeepsTheLeaseWhenTheRoleFileWriteFails(t *testing.T) {
	st := newTestStore(t)
	ref := "default/pg"
	mount := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(mount, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed mount as a plain file: %v", err)
	}
	c := New(Config{
		Leases:      lease.NewManager(st, "n2"),
		Self:        "n2",
		ResolveAddr: func(string) string { return "" },
		Promote:     func(Instance) error { return nil },
	})
	inst := Instance{BlockRef: ref, MountPath: mount, Port: 5432}
	// reconcileOne would normally read the role file first; a
	// non-existent one (mount isn't a directory, so RolePath under it
	// can't exist) falls through to elect(), not onDecided -- exercise
	// promoteOnLoss directly, the same way reconcileOne would once the
	// role file legitimately said "replica ...".
	c.promoteOnLoss(context.Background(), ref, inst, "")

	c.mu.Lock()
	_, active := c.active[ref]
	c.mu.Unlock()
	if !active {
		t.Error("a role-file write failure after a successful promotion must not abandon the lease (D5)")
	}

	// The mount recovers (e.g. the real filesystem finishes mounting
	// over it); the next Pass must find this node already active (via
	// c.active, not the unreadable role file) and repair the role file
	// without ever re-attempting TryAcquire or Promote again.
	if err := os.Remove(mount); err != nil {
		t.Fatalf("remove the stand-in file: %v", err)
	}
	if err := os.MkdirAll(mount, 0o755); err != nil {
		t.Fatalf("recreate mount as a real directory: %v", err)
	}
	promotions := 0
	c.cfg.Promote = func(Instance) error { promotions++; return nil }
	c.Pass(context.Background(), map[string]Instance{ref: inst})

	if promotions != 0 {
		t.Errorf("Promote called %d times on retry, want 0 (already active)", promotions)
	}
	if got := readRole(t, mount); got != primaryContent {
		t.Errorf("role file = %q after recovery, want %q", got, primaryContent)
	}
}

// TestReclaimPrimaryDemotesWhenAnotherNodeHoldsTheLease is Stream C's X4
// regression test: a node whose own role file still says "primary" (it
// crashed or was partitioned away) finds, on reclaim, that a DIFFERENT
// node genuinely holds the lease now -- proof another replica already
// promoted while it was gone. It must demote its own role file to
// "replica <host> <port>", the signal cmd/expanse-block-run's own
// demote-watch goroutine waits for to stop and re-clone rather than stay
// a permanently diverged primary or need a manual pg_rewind.
func TestReclaimPrimaryDemotesWhenAnotherNodeHoldsTheLease(t *testing.T) {
	st := newTestStore(t)
	ref := "default/pg"
	mount := t.TempDir()
	if err := writeRoleFile(RolePath(mount), primaryContent); err != nil {
		t.Fatalf("seed role file: %v", err)
	}
	// n3 already won the lease for real (a promotion this node never
	// heard about, having been down or partitioned).
	held, err := lease.NewManager(st, "n3").TryAcquire(context.Background(), LeaseName(ref), LeaseTTL)
	if err != nil {
		t.Fatalf("seed primary lease: %v", err)
	}
	defer held.Abandon()

	c := New(Config{
		Leases: lease.NewManager(st, "n1"),
		Self:   "n1",
		ResolveAddr: func(node string) string {
			if node == "n3" {
				return "10.42.0.9"
			}
			return ""
		},
	})
	inst := Instance{BlockRef: ref, MountPath: mount, Port: 5432}
	c.Pass(context.Background(), map[string]Instance{ref: inst})

	want := "replica 10.42.0.9 5432\n"
	if got := readRole(t, mount); got != want {
		t.Errorf("role file = %q, want %q", got, want)
	}
	c.mu.Lock()
	_, active := c.active[ref]
	c.mu.Unlock()
	if active {
		t.Error("a demoted node must not hold the primary lease")
	}
}

// TestReclaimPrimaryKeepsRetryingWhenTheWinnersAddressIsUnresolved: the
// same "not yet actionable" shape elect()'s own replica-write branch
// already handles -- a demotion must not happen (and the role file must
// stay untouched) until the new primary's node address actually
// resolves.
func TestReclaimPrimaryKeepsRetryingWhenTheWinnersAddressIsUnresolved(t *testing.T) {
	st := newTestStore(t)
	ref := "default/pg"
	mount := t.TempDir()
	if err := writeRoleFile(RolePath(mount), primaryContent); err != nil {
		t.Fatalf("seed role file: %v", err)
	}
	held, err := lease.NewManager(st, "n3").TryAcquire(context.Background(), LeaseName(ref), LeaseTTL)
	if err != nil {
		t.Fatalf("seed primary lease: %v", err)
	}
	defer held.Abandon()

	c := New(Config{
		Leases:      lease.NewManager(st, "n1"),
		Self:        "n1",
		ResolveAddr: func(string) string { return "" }, // n3's address not yet known
	})
	inst := Instance{BlockRef: ref, MountPath: mount, Port: 5432}
	c.Pass(context.Background(), map[string]Instance{ref: inst})

	if got := readRole(t, mount); got != primaryContent {
		t.Errorf("role file changed although the new primary's address is unresolved: %q", got)
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
