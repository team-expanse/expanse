package controller

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

func newStore(t *testing.T) *boltstore.Store {
	t.Helper()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "ctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func seedVolume(t *testing.T, ctx context.Context, st *boltstore.Store, id string, repl int, nodes []string, primary string, state storage.VolumeState) storage.Status {
	t.Helper()
	spec := storage.Spec{ID: id, Name: id, Replication: repl, SizeBytes: 1 << 30}
	if err := storage.SaveSpec(ctx, st, spec); err != nil {
		t.Fatal(err)
	}
	status := storage.Status{State: state, Primary: primary}
	for i, n := range nodes {
		role := storage.RoleSecondary
		if n == primary {
			role = storage.RolePrimary
		} else if i == 0 && primary == "" {
			role = storage.RolePrimary
			primary = n
		}
		status.Placement = append(status.Placement, storage.Replica{
			NodeID: n, Role: role, ZvolPath: "pool/volumes/" + id,
			Healthy: true, Sequence: uint64(10 - i),
		})
	}
	if err := storage.SaveStatus(ctx, st, id, status); err != nil {
		t.Fatal(err)
	}
	return status
}

func seedMesh(st *boltstore.Store, nodes ...string) {
	for _, n := range nodes {
		_, _ = st.Put(context.Background(),
			store.Key("/nodes/"+n+"/network.wgPublicKey"), []byte("k"))
	}
}

// TestCreateToElectedPrimaries: creation through the runtime's
// leader-gated placement, then the controller's election rule — a lost
// primary is re-elected by HIGHEST seq, ties by lowest node ID, and
// the volume is marked Degraded until recovery levels it.
func TestCreateToElectedPrimaries(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n2", "n3") // n1 permanently gone from the mesh
	// n2 has seq 12 (highest), n3 has 11.
	seedVolume(t, ctx, st, "vol-aaa", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	status, _, _ := storage.LoadStatus(ctx, st, "vol-aaa")
	status.Placement[1].Sequence = 12
	status.Placement[2].Sequence = 11
	_ = storage.SaveStatus(ctx, st, "vol-aaa", status)

	var leader atomic.Bool
	leader.Store(true)
	c := New(Options{St: st, Pool: "pool", IsLeader: leader.Load, Alert: func(AlertEvent) {}})
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, err := storage.LoadStatus(ctx, st, "vol-aaa")
	if err != nil {
		t.Fatal(err)
	}
	if got.Primary != "n2" {
		t.Fatalf("elected %q, want n2 (highest seq 12)", got.Primary)
	}
	if got.State != storage.StateDegraded {
		t.Fatalf("state = %s, want Degraded until recovery", got.State)
	}
}

// TestElectionSkipsStaleReplica: a Stale replica's higher sequence is an
// uncommitted branch (a deposed primary's local-only ops), never evidence.
func TestElectionSkipsStaleReplica(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n2", "n3") // n1 gone
	seedVolume(t, ctx, st, "vol-stale", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	status, _, _ := storage.LoadStatus(ctx, st, "vol-stale")
	status.Placement[1].Sequence = 12 // n2: highest, but Stale
	status.Placement[1].Role = storage.RoleStale
	status.Placement[2].Sequence = 11
	_ = storage.SaveStatus(ctx, st, "vol-stale", status)

	c := New(Options{St: st, Pool: "pool", IsLeader: func() bool { return true }, Alert: func(AlertEvent) {}})
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, _ := storage.LoadStatus(ctx, st, "vol-stale")
	if got.Primary != "n3" {
		t.Fatalf("elected %q, want n3 (n2 is Stale)", got.Primary)
	}
}

// TestElectionTieLowestNodeID.
func TestElectionTieLowestNodeID(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, ctx, st, "vol-tie", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	status, _, _ := storage.LoadStatus(ctx, st, "vol-tie")
	for i := range status.Placement { // all three at seq 10 → lowest ID wins
		status.Placement[i].Sequence = 10
	}
	_ = storage.SaveStatus(ctx, st, "vol-tie", status)

	c := New(Options{St: st, Pool: "pool", IsLeader: func() bool { return true }})
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, _ := storage.LoadStatus(ctx, st, "vol-tie")
	if got.Primary != "n1" {
		t.Fatalf("elected %q, want n1 (tie → lowest ID)", got.Primary)
	}
}

// TestNodeLossRebuild: a permanently-lost node's replica is replaced
// with a Resyncing row on a meshed node; scheduling is bounded to 2 in
// flight per node (a third pass does not stack a third rebuild).
func TestNodeLossRebuild(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	// Volume placed on n1 (primary), n2, n9 — n9 is permanently lost
	// (not meshed).
	seedVolume(t, ctx, st, "vol-lost", 3, []string{"n1", "n2", "n9"}, "n1", storage.StateHealthy)

	c := New(Options{St: st, Pool: "pool", IsLeader: func() bool { return true }})
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, _ := storage.LoadStatus(ctx, st, "vol-lost")
	var rebuildNode string
	for _, p := range got.Placement {
		if p.Role == storage.RoleResyncing {
			rebuildNode = p.NodeID
			if p.Healthy {
				t.Fatal("rebuild replica must start unhealthy")
			}
			if p.NodeID == "n9" {
				t.Fatal("lost node must be replaced")
			}
		}
	}
	if rebuildNode == "" {
		t.Fatal("no rebuild scheduled for the lost replica")
	}
	// Bounded concurrency: 2 rebuilds max per node — the same node
	// takes more volumes' rebuilds only up to the cap.
	if c.rebuilds[rebuildNode] != 1 {
		t.Fatalf("rebuild count = %d, want 1", c.rebuilds[rebuildNode])
	}
	c.RebuildDone(rebuildNode)
	if c.rebuilds[rebuildNode] != 0 {
		t.Fatal("RebuildDone must release the slot")
	}
}

// TestRebuildCapTwo: three rebuilds onto the same node → only 2
// scheduled.
func TestRebuildCapTwo(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2")
	for _, id := range []string{"vol-a", "vol-b", "vol-c"} {
		seedVolume(t, ctx, st, id, 2, []string{"n1", "n9"}, "n1", storage.StateHealthy)
	}
	c := New(Options{St: st, Pool: "pool", IsLeader: func() bool { return true }})
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if c.rebuilds["n2"] > 2 {
		t.Fatalf("rebuilds on n2 = %d, cap is 2", c.rebuilds["n2"])
	}
}

// TestUnderReplicationAlert: fewer healthy replicas than the factor,
// but a write quorum (2 of 3) still reachable → alert event emitted
// and state Degraded (G6.11: still readable+writable).
func TestUnderReplicationAlert(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, ctx, st, "vol-under", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	status, rev, _ := storage.LoadStatus(ctx, st, "vol-under")
	status.Placement[2].Healthy = false
	_ = storage.SaveStatus(ctx, st, "vol-under", status)
	_ = rev

	var alerts []AlertEvent
	c := New(Options{
		St: st, Pool: "pool", IsLeader: func() bool { return true },
		Alert: func(ev AlertEvent) { alerts = append(alerts, ev) },
	})
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(alerts) == 0 || alerts[0].Kind != "under-replication" || alerts[0].Have != 2 || alerts[0].Want != 3 {
		t.Fatalf("alerts = %+v, want under-replication 2/3", alerts)
	}
	got, _, _ := storage.LoadStatus(ctx, st, "vol-under")
	if got.State != storage.StateDegraded {
		t.Fatalf("state = %s, want Degraded", got.State)
	}
}

// TestReadOnlyBelowQuorum: healthy replicas drop below write quorum
// (1 of 3; quorum is floor(3/2)+1 = 2) → state ReadOnly, not Degraded
// (G6.12: writes would no longer reach quorum, so the state must say
// so — reads still work from the primary's local copy regardless).
func TestReadOnlyBelowQuorum(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, ctx, st, "vol-ro", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	status, rev, _ := storage.LoadStatus(ctx, st, "vol-ro")
	status.Placement[1].Healthy = false
	status.Placement[2].Healthy = false
	_ = storage.SaveStatus(ctx, st, "vol-ro", status)
	_ = rev

	c := New(Options{
		St: st, Pool: "pool", IsLeader: func() bool { return true },
		Alert: func(AlertEvent) {},
	})
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, _ := storage.LoadStatus(ctx, st, "vol-ro")
	if got.State != storage.StateReadOnly {
		t.Fatalf("state = %s, want ReadOnly", got.State)
	}

	// Restore both — state must recover to Healthy, not stay stuck.
	status, rev, _ = storage.LoadStatus(ctx, st, "vol-ro")
	status.Placement[1].Healthy = true
	status.Placement[2].Healthy = true
	_ = storage.SaveStatus(ctx, st, "vol-ro", status)
	_ = rev
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, _ = storage.LoadStatus(ctx, st, "vol-ro")
	if got.State != storage.StateHealthy {
		t.Fatalf("state after recovery = %s, want Healthy", got.State)
	}
}

// TestDeleteEndToEnd: Delete marks Deleting; the next reconcile
// destroys zvols and drops both records.
func TestDeleteEndToEnd(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, ctx, st, "vol-del", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	destroyed := map[string]bool{}
	c := New(Options{
		St: st, Pool: "pool", IsLeader: func() bool { return true },
		ZFS: fakeCtlZFS{destroyed: destroyed},
	})
	if err := c.Delete(ctx, "vol-del"); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if !destroyed["pool/volumes/vol-del"] {
		t.Fatalf("zvols not destroyed: %v", destroyed)
	}
	if _, _, err := storage.LoadStatus(ctx, st, "vol-del"); err == nil {
		t.Fatal("status record should be gone")
	}
	if _, err := storage.LoadSpec(ctx, st, "vol-del"); err == nil {
		t.Fatal("spec record should be gone")
	}
}

type fakeCtlZFS struct{ destroyed map[string]bool }

func (f fakeCtlZFS) DestroyZvol(_ context.Context, zvol string, _ bool) error {
	f.destroyed[zvol] = true
	return nil
}
func (f fakeCtlZFS) Scrub(_ context.Context, _ string) error { return nil }

// TestScrubMonthly: the scrub runs once per interval, not per tick.
func TestScrubMonthly(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	var scrubs atomic.Int32
	c := New(Options{
		St: st, Pool: "pool", IsLeader: func() bool { return true },
		ScrubInterval: time.Hour,
		ZFS:           scrubZFS{fn: func() { scrubs.Add(1) }},
	})
	for i := 0; i < 5; i++ {
		if err := c.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if scrubs.Load() != 1 {
		t.Fatalf("scrubs = %d, want 1 per interval", scrubs.Load())
	}
}

type scrubZFS struct{ fn func() }

func (s scrubZFS) DestroyZvol(context.Context, string, bool) error { return nil }
func (s scrubZFS) Scrub(context.Context, string) error {
	s.fn()
	return nil
}

// TestNeedsManualRecoveryUntouched: §9 — the controller never touches
// a diverged volume.
func TestNeedsManualRecoveryUntouched(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, ctx, st, "vol-div", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateNeedsManualRecovery)
	c := New(Options{St: st, Pool: "pool", IsLeader: func() bool { return true }})
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, _ := storage.LoadStatus(ctx, st, "vol-div")
	if got.State != storage.StateNeedsManualRecovery || got.Primary != "n1" {
		t.Fatalf("controller modified a diverged volume: %+v", got)
	}
}

// TestNonLeaderInert: a non-leader controller never mutates state.
func TestNonLeaderInert(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2")
	seedVolume(t, ctx, st, "vol-x", 2, []string{"n1", "n2"}, "", storage.StateHealthy)
	c := New(Options{St: st, Pool: "pool", IsLeader: func() bool { return false }})
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, _ := storage.LoadStatus(ctx, st, "vol-x")
	if got.Primary != "" {
		t.Fatalf("non-leader elected a primary: %+v", got)
	}
}

// TestOpsDeleteAndMovePrimary (T15 §4.8): the op records the ctl writes
// are consumed by the leader's controller — delete by volume NAME, and
// move-primary only to a node that already holds a replica.
func TestOpsDeleteAndMovePrimary(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, ctx, st, "vol-op", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	destroyed := map[string]bool{}
	c := New(Options{
		St: st, Pool: "pool", IsLeader: func() bool { return true },
		ZFS: fakeCtlZFS{destroyed: destroyed},
	})

	// move-primary: legal target (n2 holds a replica).
	op := []byte(`{"target":"vol-op","to":"n2"}`)
	if _, err := st.Put(ctx, store.Key("/volumes/_ops/move-primary/vol-op"), op); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	status, _, err := storage.LoadStatus(ctx, st, "vol-op")
	if err != nil {
		t.Fatal(err)
	}
	if status.Primary != "n2" {
		t.Fatalf("primary = %s, want n2", status.Primary)
	}
	if _, err := st.Get(ctx, store.Key("/volumes/_ops/move-primary/vol-op")); err == nil {
		t.Fatal("move-primary op not consumed")
	}

	// move-primary refused: n9 holds no replica.
	if _, err := st.Put(ctx, store.Key("/volumes/_ops/move-primary/vol-op"), []byte(`{"target":"vol-op","to":"n9"}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if status, _, _ = storage.LoadStatus(ctx, st, "vol-op"); status.Primary != "n2" {
		t.Fatalf("primary moved to non-replica node: %s", status.Primary)
	}

	// delete by NAME → volume gone + zvol destroyed.
	if _, err := st.Put(ctx, store.Key("/volumes/_ops/delete/vol-op"), []byte(`{"target":"vol-op"}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.LoadStatus(ctx, st, "vol-op"); err == nil {
		t.Fatal("volume should be deleted via op record")
	}
	if !destroyed["pool/volumes/vol-op"] {
		t.Fatal("zvol not destroyed via delete op")
	}
}

// TestExpiredLeaseReElectsPrimary: a HARD-killed primary never
// unpublishes its mesh record, so record presence alone cannot mean
// "alive". Once the node's liveness lease (/leases/node-<id>) has
// expired, the controller must re-elect (highest-seq rule) even though
// the record is still there.
func TestExpiredLeaseReElectsPrimary(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, ctx, st, "vol-dead", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	status, _, _ := storage.LoadStatus(ctx, st, "vol-dead")
	status.Placement[2].Sequence = 11 // n3 highest surviving seq
	_ = storage.SaveStatus(ctx, st, "vol-dead", status)

	// n1's liveness lease: granted, then expired (hard-kill).
	expired := []byte(`{"h":"n1","e":-1}`)
	if _, err := st.Put(ctx, store.Key("/leases/node-n1"), expired); err != nil {
		t.Fatal(err)
	}

	var leader atomic.Bool
	leader.Store(true)
	c := New(Options{St: st, Pool: "pool", IsLeader: leader.Load, Alert: func(AlertEvent) {}})
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, err := storage.LoadStatus(ctx, st, "vol-dead")
	if err != nil {
		t.Fatal(err)
	}
	if got.Primary != "n3" {
		t.Fatalf("primary = %s, want n3 (highest-seq re-election after lease expiry)", got.Primary)
	}
	if got.State != storage.StateDegraded {
		t.Fatalf("state = %s, want Degraded until recovery levels it", got.State)
	}
}

// TestElectionRequiresLiveProbeEvidence (§4.3 step 2, T17.4): with
// ProbeSeq wired, a candidate must present LIVE evidence that its
// durable copy reaches the highest probed sequence. The store's
// placement.Sequence is asynchronous — here it names n3 (11), but
// n3's probe fails (its zvol is gone) and n2 proves seq 9: n2 wins,
// the dishonest placement row loses.
func TestElectionRequiresLiveProbeEvidence(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, ctx, st, "vol-probe", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	status, _, _ := storage.LoadStatus(ctx, st, "vol-probe")
	status.Placement[2].Sequence = 11 // n3 claims the highest seq...
	_ = storage.SaveStatus(ctx, st, "vol-dead", status)
	status, _, _ = storage.LoadStatus(ctx, st, "vol-probe")
	_ = status

	// n1's liveness lease: expired (hard kill).
	expired := []byte(`{"h":"n1","e":-1}`)
	if _, err := st.Put(ctx, store.Key("/leases/node-n1"), expired); err != nil {
		t.Fatal(err)
	}

	probes := map[string]struct {
		seq uint64
		err error
	}{
		"n1": {0, context.DeadlineExceeded}, // dead primary: unreachable
		"n2": {9, nil},                      // live, current
		"n3": {0, context.DeadlineExceeded}, // meshed but zvol lost
	}
	c := New(Options{
		St: st, Pool: "pool", IsLeader: func() bool { return true },
		ProbeSeq: func(ctx context.Context, volID, node string) (uint64, error) {
			p := probes[node]
			return p.seq, p.err
		},
		Alert: func(AlertEvent) {},
	})
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, err := storage.LoadStatus(ctx, st, "vol-probe")
	if err != nil {
		t.Fatal(err)
	}
	if got.Primary != "n2" {
		t.Fatalf("primary = %s, want n2 (only candidate with live evidence at the highest probed seq)", got.Primary)
	}
}

// TestElectionNoLiveCandidateRefuses (§4.3/§9, T17.4): when no
// candidate presents live probe evidence, election must NOT hand the
// volume to an unproven node; after NoCandidateRounds consecutive
// evidence-less rounds the volume is flagged NeedsManualRecovery.
func TestElectionNoLiveCandidateRefuses(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, ctx, st, "vol-none", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	expired := []byte(`{"h":"n1","e":-1}`)
	if _, err := st.Put(ctx, store.Key("/leases/node-n1"), expired); err != nil {
		t.Fatal(err)
	}

	var alerts []AlertEvent
	c := New(Options{
		St: st, Pool: "pool", IsLeader: func() bool { return true },
		ProbeSeq: func(ctx context.Context, volID, node string) (uint64, error) {
			return 0, context.DeadlineExceeded // nobody answers honestly
		},
		NoCandidateRounds: 2,
		Alert:             func(e AlertEvent) { alerts = append(alerts, e) },
	})
	for i := 0; i < 2; i++ {
		if err := c.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got, _, err := storage.LoadStatus(ctx, st, "vol-none")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != storage.StateNeedsManualRecovery {
		t.Fatalf("state = %s, want NeedsManualRecovery (no live candidate)", got.State)
	}
	if got.Primary != "n1" {
		t.Fatalf("primary = %s, want unchanged n1 (refusal must not elect an unproven node)", got.Primary)
	}
	if len(alerts) == 0 {
		t.Fatal("expected a no-current-candidate alert")
	}
}

// TestElectionHysteresisNoChurn (T17.4): an election round that would
// change nothing (same primary, same state) must not CAS the status —
// revision churn from a sticky re-election re-triggers the runtimes'
// demotion paths every tick (the VM flap).
func TestElectionHysteresisNoChurn(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, ctx, st, "vol-hyst", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	// n1's VOLUME lease expired (fence lapsed) but its LIVENESS lease
	// is current (it is meshed), and the probes confirm n1 is STILL
	// current — the round must be a no-op, not a churn CAS.
	live := []byte(`{"h":"n1","e":9223372036854775807}`)
	if _, err := st.Put(ctx, store.Key("/leases/node-n1"), live); err != nil {
		t.Fatal(err)
	}
	expired := []byte(`{"h":"n1","e":-1}`)
	if _, err := st.Put(ctx, store.Key("/leases/exvol-vol-vol-hyst"), expired); err != nil {
		t.Fatal(err)
	}
	before, beforeRev, err := storage.LoadStatus(ctx, st, "vol-hyst")
	if err != nil {
		t.Fatal(err)
	}

	c := New(Options{
		St: st, Pool: "pool", IsLeader: func() bool { return true },
		ProbeSeq: func(ctx context.Context, volID, node string) (uint64, error) {
			return uint64(10), nil // everyone current; n1 wins the tie
		},
		Alert: func(AlertEvent) {},
	})
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	after, afterRev, err := storage.LoadStatus(ctx, st, "vol-hyst")
	if err != nil {
		t.Fatal(err)
	}
	if after.Primary != before.Primary || after.State != before.State {
		t.Fatalf("status changed unexpectedly: primary=%s state=%s (was %s/%s)",
			after.Primary, after.State, before.Primary, before.State)
	}
	if afterRev != beforeRev {
		t.Fatalf("revision %d → %d: hysteresis violated (CAS without a delta)", beforeRev, afterRev)
	}
}

// A latch set because no node presented evidence (a control-plane blackout, not
// data loss) must lift itself once a meshed node answers, and election must resume.
func TestNoCandidateLatchLiftsWhenANodeAnswers(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, ctx, st, "vol-lift", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	if _, err := st.Put(ctx, store.Key("/leases/node-n1"), []byte(`{"h":"n1","e":-1}`)); err != nil {
		t.Fatal(err)
	}

	answering := false
	c := New(Options{
		St: st, Pool: "pool", IsLeader: func() bool { return true },
		ProbeSeq: func(ctx context.Context, volID, node string) (uint64, error) {
			if answering && node != "n1" {
				return 7, nil
			}
			return 0, context.DeadlineExceeded
		},
		NoCandidateRounds: 2,
		Alert:             func(AlertEvent) {},
	})
	for i := 0; i < 2; i++ {
		if err := c.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got, _, _ := storage.LoadStatus(ctx, st, "vol-lift"); got.State != storage.StateNeedsManualRecovery {
		t.Fatalf("setup: state = %s, want the latch set", got.State)
	}

	answering = true
	for i := 0; i < 3; i++ {
		if err := c.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got, _, _ := storage.LoadStatus(ctx, st, "vol-lift")
	if got.State == storage.StateNeedsManualRecovery || got.Primary == "n1" {
		t.Fatalf("still latched or unelected: state=%s primary=%s (a live node answers probes)", got.State, got.Primary)
	}
}

// Refusing to elect is right when the controller cannot see enough nodes, but
// that is a gap in its view, not evidence of data loss: it must never latch.
func TestRefusalWithTooFewProbedNodesNeverLatches(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, ctx, st, "vol-blind", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	for _, n := range []string{"n1", "n2", "n3"} { // every liveness lease looks lapsed (control-plane blackout)
		if _, err := st.Put(ctx, store.Key("/leases/node-"+n), []byte(`{"h":"`+n+`","e":-1}`)); err != nil {
			t.Fatal(err)
		}
	}
	alerts := 0
	c := New(Options{
		St: st, Pool: "pool", IsLeader: func() bool { return true },
		ProbeSeq:          func(context.Context, string, string) (uint64, error) { return 0, context.DeadlineExceeded },
		NoCandidateRounds: 2,
		Alert:             func(AlertEvent) { alerts++ },
	})
	for i := 0; i < 6; i++ {
		if err := c.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got, _, _ := storage.LoadStatus(ctx, st, "vol-blind")
	if got.State == storage.StateNeedsManualRecovery {
		t.Fatal("latched although no node could even be probed: that is blindness, not data loss")
	}
	if got.Primary != "n1" {
		t.Fatalf("primary = %s: an unproven node must not be elected", got.Primary)
	}
	if alerts == 0 {
		t.Fatal("the refusal must still be reported")
	}
}

// The latch outlives the controller that set it: after a leader change the new
// controller must lift a no-candidate latch, but never a divergence latch.
func TestAutoLatchLiftsAcrossControllersButDivergenceStays(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, ctx, st, "vol-auto", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	seedVolume(t, ctx, st, "vol-div", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	if _, err := st.Put(ctx, store.Key("/leases/node-n1"), []byte(`{"h":"n1","e":-1}`)); err != nil {
		t.Fatal(err)
	}
	dead := func(context.Context, string, string) (uint64, error) { return 0, context.DeadlineExceeded }
	old := New(Options{St: st, Pool: "pool", IsLeader: func() bool { return true }, ProbeSeq: dead, NoCandidateRounds: 2, Alert: func(AlertEvent) {}})
	for i := 0; i < 2; i++ {
		if err := old.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// vol-div is latched by a divergence (no marker), like the runtime's markDiverged.
	div, rev, _ := storage.LoadStatus(ctx, st, "vol-div")
	div.State = storage.StateNeedsManualRecovery
	if err := storage.CompareAndSwapStatus(ctx, st, "vol-div", rev, div); err != nil {
		t.Fatal(err)
	}
	storage.ClearAutoLatch(ctx, st, "vol-div")

	fresh := New(Options{
		St: st, Pool: "pool", IsLeader: func() bool { return true }, Alert: func(AlertEvent) {},
		ProbeSeq: func(_ context.Context, _, node string) (uint64, error) {
			if node == "n1" {
				return 0, context.DeadlineExceeded
			}
			return 7, nil
		},
	})
	for i := 0; i < 3; i++ {
		if err := fresh.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got, _, _ := storage.LoadStatus(ctx, st, "vol-auto"); got.State == storage.StateNeedsManualRecovery || got.Primary == "n1" {
		t.Fatalf("vol-auto: state=%s primary=%s: a new controller must lift its predecessor's no-candidate latch", got.State, got.Primary)
	}
	if got, _, _ := storage.LoadStatus(ctx, st, "vol-div"); got.State != storage.StateNeedsManualRecovery {
		t.Fatalf("vol-div: state=%s: a divergence latch is for a human", got.State)
	}
}
