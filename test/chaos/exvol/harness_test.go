package exvol

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/cluster/lease"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
)

const recSize = 4096

// recData builds a distinct 4K record body.
func recData(i int) []byte {
	b := make([]byte, recSize)
	for j := range b {
		b[j] = byte((i*7 + j*3) % 251)
	}
	copy(b, []byte("rec-"))
	copy(b[4:], []byte(fmtI(i)))
	return b
}

func fmtI(i int) string { return fmt.Sprintf("%d", i) }

// zvolFile returns a node's local zvol file path for volID.
func (n *Node) zvolFile(volID string) string {
	return filepath.Join(n.Root, "pool", "dev", pool, "volumes", volID)
}

// fileHas reports whether the node's zvol file holds the record.
func (n *Node) fileHas(t *testing.T, volID string, i int) bool {
	t.Helper()
	want := recData(i)
	f, err := os.Open(n.zvolFile(volID))
	if err != nil {
		return false
	}
	defer f.Close() //nolint:errcheck
	buf := make([]byte, recSize)
	if _, err := f.ReadAt(buf, int64(i*recSize)); err != nil {
		return false
	}
	return bytes.Equal(buf, want)
}

// writeAcks drives write+flush of one record through the live primary.
func (c *Cluster) writeAcks(volID string, i int) error {
	if err := c.Write(volID, int64(i*recSize), recData(i)); err != nil {
		return err
	}
	return c.Flush(volID)
}

// TestFailoverPreservesAckedRecords is the release blocker (§6 G6.3):
// write acked records, kill the primary, fail over, verify every
// acked record is intact on the new primary, restore the old primary,
// and require three-way checksum equality.
func TestFailoverPreservesAckedRecords(t *testing.T) {
	const (
		volID = "vol-harness1"
		recs  = 8
	)
	c := NewCluster(t, "n1", "n2", "n3")
	c.CreateVolume(volID, 1<<20, []string{"n1", "n2", "n3"})

	// Wait for promotion, then write+flush the records (each acked).
	var lastErr error
	waitFor(t, 15*time.Second, "first record acked on n1", func() bool {
		lastErr = c.writeAcks(volID, 0)
		return lastErr == nil
	})
	for i := 1; i < recs; i++ {
		if err := c.writeAcks(volID, i); err != nil {
			t.Fatalf("record %d: %v", i+1, err)
		}
	}

	// Every acked record must be readable on the crashed primary AND
	// reachable replicas before the kill.
	for _, id := range []string{"n1", "n2", "n3"} {
		for i := 0; i < recs; i++ {
			if !c.byID[id].fileHas(t, volID, i) {
				t.Fatalf("pre-kill: record %d missing on %s", i+1, id)
			}
		}
	}

	// Hard-kill the primary; its lease record expires (≤ TTL).
	c.Crash("n1")

	// The controller re-elects: n2 takes over (n3 has the same data;
	// either would satisfy the blocker — assert whichever the test
	// elects still serves every record).
	c.Elect(volID, "n2")

	i := 0
	waitFor(t, 20*time.Second, "new primary acks writes", func() bool {
		err := c.writeAcks(volID, recs+i)
		if err == nil {
			i++
			return i > 0
		}
		lastErr = err
		return false
	})
	_ = lastErr

	// Every pre-crash record must survive on the new primary (and on
	// n3, which never went down).
	for _, id := range []string{"n2", "n3"} {
		for r := 0; r < recs; r++ {
			if !c.byID[id].fileHas(t, volID, r) {
				t.Fatalf("post-kill: acked record %d LOST on %s (release blocker)", r+1, id)
			}
		}
	}

	// Restore the old primary: it must resync, not serve its stale
	// branch (§4.3 recovery + resync).
	c.Restart("n1")
	waitFor(t, 20*time.Second, "n1 resynced to the new primary's seq", func() bool {
		return c.byID["n1"].fileHas(t, volID, recs) // the post-failover record
	})

	// Three-way equality on every record.
	for _, id := range []string{"n1", "n2", "n3"} {
		for r := 0; r < recs+i; r++ {
			if !c.byID[id].fileHas(t, volID, r) {
				t.Fatalf("final: record %d diverged on %s", r+1, id)
			}
		}
	}
}

// TestRestartedReplicaOplogSurvives: a daemon restart must not forget
// which sequences its zvol holds — the durable oplog reloads and the
// replica can still serve FetchOps for the ops it holds.
func TestRestartedReplicaOplogSurvives(t *testing.T) {
	const volID = "vol-oplog"
	c := NewCluster(t, "n1", "n2", "n3")
	c.CreateVolume(volID, 1<<20, []string{"n1", "n2", "n3"})

	waitFor(t, 15*time.Second, "first record acked", func() bool {
		return c.writeAcks(volID, 0) == nil
	})
	if err := c.writeAcks(volID, 1); err != nil {
		t.Fatal(err)
	}

	// The oplog must exist BEFORE the restart (fsynced at flush).
	oplog := filepath.Join(c.byID["n2"].Root, "pool", ".oplogs", volID+".oplog")
	if _, err := os.Stat(oplog); err != nil {
		t.Fatalf("n2 oplog missing pre-crash at %s: %v", oplog, err)
	}

	// Graceful restart of a secondary: oplog must reload (§4.3 4a).
	c.Crash("n2")
	c.Restart("n2")

	// A later failover to n3 must be able to pull ops for the
	// restarted n2 — i.e. n2's oplog knows seq 1..4.
	waitFor(t, 10*time.Second, "n2 oplog reloaded", func() bool {
		oplog := filepath.Join(c.byID["n2"].Root, "pool", ".oplogs", volID+".oplog")
		raw, err := os.ReadFile(oplog)
		return err == nil && bytes.Contains(raw, []byte("\n")) && len(raw) > 0
	})
}

// TestTornZvolReplicaIsStaledNotFatal: a replica whose oplog CLAIMS
// sequences but whose zvol bytes are gone (torn write / lost zvol with
// surviving oplog — the exact VM failure "fetch op 1 from n3: EOF")
// must be treated as a DISHONEST HOLDER: excluded from recovery
// sourcing, staled, and rebuilt via resync — never abort the failover
// and never serve fabricated zeros.
func TestTornZvolReplicaIsStaledNotFatal(t *testing.T) {
	const (
		volID = "vol-torn"
		recs  = 4
	)
	c := NewCluster(t, "n1", "n2", "n3")
	c.CreateVolume(volID, 1<<20, []string{"n1", "n2", "n3"})

	waitFor(t, 15*time.Second, "first record acked", func() bool {
		return c.writeAcks(volID, 0) == nil
	})
	for i := 1; i < recs; i++ {
		if err := c.writeAcks(volID, i); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"n1", "n2", "n3"} {
		if !c.byID[id].fileHas(t, volID, recs-1) {
			t.Fatalf("pre-kill: record %d missing on %s", recs, id)
		}
	}

	// Tear n2's zvol: the oplog still claims seqs 1..4, but the bytes
	// are gone. Reading fails honestly; serving zeros is the bug.
	if err := os.Truncate(c.byID["n2"].zvolFile(volID), 0); err != nil {
		t.Fatal(err)
	}

	// Fail over: the new primary's recovery must NOT abort on n2's
	// dishonesty (the old code flapped: "fetch ops ...: EOF" forever).
	c.Crash("n1")
	c.Elect(volID, "n3")
	waitFor(t, 20*time.Second, "n3 serving as recovered primary", func() bool {
		return c.writeAcks(volID, recs) == nil
	})

	// Bring the crashed n1 back: BOTH stale replicas (n1 unreachable,
	// n2 leveled-fail) must rebuild via resync now that recovery's
	// stale marks reach the coordinator.
	c.Restart("n1")

	// The torn replica must be backfilled by resync — not fed the
	// failover, and not left blocking quorum forever.
	waitFor(t, 30*time.Second, "n2 torn zvol rebuilt via resync", func() bool {
		return c.byID["n2"].fileHas(t, volID, recs) && c.byID["n2"].fileHas(t, volID, recs-1)
	})

	// n1 (crashed through the failover) rebuilds via adopt + resync.
	waitFor(t, 30*time.Second, "n1 resynced to the new primary's seq", func() bool {
		return c.byID["n1"].fileHas(t, volID, recs)
	})

	// Three-way equality restored; writes still acked at quorum.
	for _, id := range []string{"n1", "n2", "n3"} {
		for r := 0; r <= recs; r++ {
			if !c.byID[id].fileHas(t, volID, r) {
				t.Fatalf("final: record %d wrong on %s", r+1, id)
			}
		}
	}
}

// TestUnfillableClaimMeansManualRecovery: when a claimed op cannot be
// served honestly by ANY holder, recovery must stop the automatic
// retry loop and flag NeedsManualRecovery (suspected data loss of
// claimed durability) — not flap forever, and never fabricate bytes.
func TestUnfillableClaimMeansManualRecovery(t *testing.T) {
	const (
		volID = "vol-unfillable"
		recs  = 4
	)
	c := NewCluster(t, "n1", "n2", "n3")
	c.CreateVolume(volID, 1<<20, []string{"n1", "n2", "n3"})

	// n2 is down BEFORE the writes: quorum is n1+n3, so n2 has nothing
	// to give back (it must not be a fallback source).
	c.Crash("n2")
	time.Sleep(3 * time.Second) // let the lease lapse; keep n2 fully out

	waitFor(t, 15*time.Second, "first record acked (n1+n3 quorum)", func() bool {
		return c.writeAcks(volID, 0) == nil
	})
	for i := 1; i < recs; i++ {
		if err := c.writeAcks(volID, i); err != nil {
			t.Fatal(err)
		}
	}
	if c.byID["n2"].fileHas(t, volID, 0) {
		t.Fatal("n2 was down; it must not hold record 1")
	}

	// n1 — the primary that wrote seqs 1..4 — durably holds them too
	// (§4.3 4a: a node's own writes made while primary survive into its
	// own oplog, recovered by *this same fix*). Crash it first so its
	// zvol is safe to truncate (not mid-write under a live primary),
	// then destroy both its and n3's zvols — oplogs still claim 1..4,
	// bytes are gone everywhere. n2 never held them (down pre-writes).
	c.Crash("n1")
	if err := os.Truncate(c.byID["n1"].zvolFile(volID), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(c.byID["n3"].zvolFile(volID), 0); err != nil {
		t.Fatal(err)
	}

	// Fail over to n2: no node can honestly serve seqs 1..4 any more —
	// unfillable. n2 was crashed pre-writes; its restart brings the
	// empty branch node back up.
	c.Restart("n2")
	c.Elect(volID, "n2")
	waitFor(t, 20*time.Second, "volume flagged NeedsManualRecovery", func() bool {
		return c.Status(volID).State == storage.StateNeedsManualRecovery
	})

	// And n2 must NOT serve writes on the unfillable branch.
	if err := c.Write(volID, int64(recs*recSize), recData(recs)); err == nil {
		t.Fatal("n2 served writes on an unfillable (suspected-loss) branch")
	}
}

// revokeLease hands the volume lease to a ghost holder with a
// far-future expiry — simulating a takeover by a node the current
// primary cannot see (the fence that must demote a serving primary).
func revokeLease(c *Cluster, volID, newHolder string) {
	c.T.Helper()
	key := store.Key(lease.Prefix + "exvol-vol-" + volID)
	val, err := lease.EncodeForTest(lease.Lease{
		Name: "exvol-vol-" + volID, Holder: newHolder,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		c.T.Fatal(err)
	}
	for i := 0; i < 10; i++ { // race the holder's renewals for the CAS
		cur, gerr := c.St.Get(c.ctx(), key)
		if gerr != nil {
			c.T.Fatal(gerr)
		}
		if _, err := c.St.CompareAndSwap(c.ctx(), key, cur.Revision, val); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.T.Fatal("could not overwrite the volume lease record")
}

// TestLeaseRevocationDemotesPrimary (§4.3, T17.3): a serving primary
// whose volume lease is taken over must stop serving within a tick or
// two of learning the fact — writes must start failing, not keep
// acking on a stolen fence (split brain).
func TestLeaseRevocationDemotesPrimary(t *testing.T) {
	const (
		volID = "vol-revoke"
		recs  = 2
	)
	c := NewCluster(t, "n1", "n2", "n3")
	c.CreateVolume(volID, 1<<20, []string{"n1", "n2", "n3"})

	waitFor(t, 15*time.Second, "first record acked", func() bool {
		return c.writeAcks(volID, 0) == nil
	})
	if err := c.writeAcks(volID, recs-1); err != nil {
		t.Fatal(err)
	}

	revokeLease(c, volID, "ghost")

	waitFor(t, 10*time.Second, "demoted primary refuses writes", func() bool {
		return c.Write(volID, int64(recs*recSize), recData(recs)) != nil
	})
	// The refusal must persist (the ghost holds the fence for 1h) —
	// a demote-then-silently-repromote would still be split brain.
	time.Sleep(2 * time.Second)
	if err := c.Write(volID, int64(recs*recSize), recData(recs)); err == nil {
		t.Fatal("primary resumed serving on a revoked lease")
	}
}

// TestReElectionDemotesOldPrimary (§4.3, T17.3): when the store's
// status row moves to a new primary WITHOUT the old one crashing, the
// old primary must step down (status watchdog) and join as a replica
// of the new one — both must never serve concurrently.
func TestReElectionDemotesOldPrimary(t *testing.T) {
	const (
		volID = "vol-move"
		recs  = 2
	)
	c := NewCluster(t, "n1", "n2", "n3")
	c.CreateVolume(volID, 1<<20, []string{"n1", "n2", "n3"})

	waitFor(t, 15*time.Second, "first record acked", func() bool {
		return c.writeAcks(volID, 0) == nil
	})
	if err := c.writeAcks(volID, recs-1); err != nil {
		t.Fatal(err)
	}

	// Pure control-plane re-election: no crash, n1 keeps running.
	c.Elect(volID, "n2")

	// The new primary must reach serving (n1's step-down releases the
	// fence within TTL) and the post-move write must be fanned out to
	// the OLD primary as a replica — proof it rejoined, not split off.
	waitFor(t, 20*time.Second, "post-move write acked on n2", func() bool {
		return c.writeAcks(volID, recs) == nil
	})
	waitFor(t, 15*time.Second, "old primary rejoined as replica", func() bool {
		return c.byID["n1"].fileHas(t, volID, recs)
	})
	// A stale device left attached on the demoted node is exactly what
	// made a fully-recovered vol-durability.nix run look "no ready
	// primary" forever: the CLI/§4.7 double-primary check treats device
	// attachment as the signal for "serving as primary", so a leftover
	// attach on n1 makes it look like two nodes are still primary.
	waitFor(t, 10*time.Second, "old primary's device detached", func() bool {
		return !c.byID["n1"].DeviceAttached()
	})
}

// TestTornPrimarySelfHeals (§4.3, T17.3): the currency watchdog — a
// serving primary that can no longer re-read its own acked bytes
// (torn/zeroed region) must step down; re-promotion re-runs recovery,
// which re-pulls the missing ops from the replicas and heals the copy
// WITHOUT any failover.
func TestTornPrimarySelfHeals(t *testing.T) {
	const (
		volID = "vol-selfheal"
		recs  = 3
	)
	c := NewCluster(t, "n1", "n2", "n3")
	c.CreateVolume(volID, 1<<20, []string{"n1", "n2", "n3"})

	waitFor(t, 15*time.Second, "first record acked", func() bool {
		return c.writeAcks(volID, 0) == nil
	})
	for i := 1; i < recs; i++ {
		if err := c.writeAcks(volID, i); err != nil {
			t.Fatal(err)
		}
	}

	// Zero the primary's own copy of record recs-1 — the storage fault
	// that reads back as success (no error, wrong bytes).
	f, err := os.OpenFile(c.byID["n1"].zvolFile(volID), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, werr := f.WriteAt(make([]byte, recSize), int64((recs-1)*recSize)); werr != nil {
		t.Fatal(werr)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// Watchdog steps the primary down; re-promotion heals from n2/n3.
	waitFor(t, 25*time.Second, "primary healed and serving again", func() bool {
		return c.writeAcks(volID, recs) == nil
	})
	waitFor(t, 10*time.Second, "torn bytes restored on the primary", func() bool {
		return c.byID["n1"].fileHas(t, volID, recs-1)
	})
}

// TestDurabilityLoop is the in-process analog of vol-durability.nix's
// release-blocker loop (§6 G6.3): write acked records, crash the
// primary, fail over, verify every acked record, restore the crashed
// node, require 3-way checksum equality — repeated many times in one
// process instead of one cycle per ~70s VM boot. It is the fast tool
// for catching this class of regression (this package's device-detach
// bug, the resync snapshot numeric-ordering bug at seq>=10, etc.)
// long before a VM run would surface it; the VM test remains the
// final gate against the real zfs/systemd/WireGuard stack.
//
// Election here is driven by c.Elect (a direct status CAS) rather
// than a real raft-leader controller — this harness has no controller
// loop (see package doc) — so it exercises the runtime's own recovery
// and role-change plumbing, not the controller's election algorithm
// (covered separately by internal/storage/controller's tests).
func TestDurabilityLoop(t *testing.T) {
	const (
		volID = "vol-durloop"
		iters = 15
	)
	c := NewCluster(t, "n1", "n2", "n3")
	c.CreateVolume(volID, 1<<20, []string{"n1", "n2", "n3"})
	nodes := []string{"n1", "n2", "n3"}
	nextIdx := map[string]int{"n1": 1, "n2": 2, "n3": 0}

	acked := 0
	waitFor(t, 15*time.Second, "first record acked", func() bool {
		return c.writeAcks(volID, acked) == nil
	})
	acked++

	for it := 0; it < iters; it++ {
		for n := 0; n < 1+it%2; n++ { // 1-2 records this iteration
			if err := c.writeAcks(volID, acked); err != nil {
				t.Fatalf("iter %d: write %d: %v", it, acked, err)
			}
			acked++
		}

		primary := c.Status(volID).Primary
		c.Crash(primary)
		next := nodes[nextIdx[primary]]
		c.Elect(volID, next)

		waitFor(t, 90*time.Second, fmt.Sprintf("iter %d: %s serves", it, next), func() bool {
			return c.writeAcks(volID, acked) == nil
		})
		acked++

		for _, id := range nodes {
			if id == primary {
				continue // crashed; checked after its restart below
			}
			for r := 0; r < acked; r++ {
				if !c.byID[id].fileHas(t, volID, r) {
					t.Fatalf("iter %d: ACKED RECORD %d LOST on %s (release blocker)", it, r, id)
				}
			}
		}

		c.Restart(primary)
		waitFor(t, 15*time.Second, fmt.Sprintf("iter %d: %s resynced", it, primary), func() bool {
			return c.byID[primary].fileHas(t, volID, acked-1)
		})
		// The byte-level resync landing is not the same moment the
		// restarted node's secondary role/oplog fully settles; crashing
		// it again as someone else's leveling target immediately after
		// is a race this loop's tight cadence can hit but no real
		// timeline would (SnapshotInterval alone is 1s in this harness).
		time.Sleep(500 * time.Millisecond)
	}

	for _, id := range nodes {
		for r := 0; r < acked; r++ {
			if !c.byID[id].fileHas(t, volID, r) {
				t.Fatalf("final: record %d diverged on %s", r, id)
			}
		}
	}
}

// TestSecondaryReconnectResyncs covers a scenario none of the above do:
// the PRIMARY never changes. A secondary drops (daemon stop, not a
// primary failover), the primary keeps taking acked writes with the
// remaining quorum, and the secondary comes back. vol-resync-incremental
// .nix hit exactly this shape and found the restarted secondary
// reported "secondary" immediately and never actually received the
// writes it missed — this reproduces that in under a second.
func TestSecondaryReconnectResyncs(t *testing.T) {
	const (
		volID = "vol-secreconnect"
		pre   = 3 // acked before the secondary drops
		drift = 5 // acked while the secondary is down
	)
	c := NewCluster(t, "n1", "n2", "n3")
	c.CreateVolume(volID, 1<<20, []string{"n1", "n2", "n3"})

	waitFor(t, 15*time.Second, "first record acked", func() bool {
		return c.writeAcks(volID, 0) == nil
	})
	for i := 1; i < pre; i++ {
		if err := c.writeAcks(volID, i); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 10*time.Second, "n3 has the pre-crash baseline", func() bool {
		return c.byID["n3"].fileHas(t, volID, pre-1)
	})

	// Let >=2 periodic @resync-<seq> snapshots accumulate before the
	// crash (SnapshotInterval is 1s in this harness) — the VM run that
	// found this had several minutes and multiple periodic snapshots by
	// the time its secondary went stale; a resync choosing among
	// several existing snapshots is a different code path than picking
	// the only one that has ever existed.
	time.Sleep(2500 * time.Millisecond)

	// n3 is a secondary throughout (n1 is primary and stays primary).
	c.Crash("n3")
	for i := 0; i < drift; i++ {
		if err := c.writeAcks(volID, pre+i); err != nil {
			t.Fatalf("drift write %d (quorum n1+n2 only): %v", i, err)
		}
	}

	c.Restart("n3")

	// The bug: status.Placement never marks a live-write-excluded
	// replica Stale, so this reads "secondary" from the moment n3's
	// daemon is back — not from when it actually catches up. Wait on
	// the thing that actually matters instead: the missing bytes
	// landing on n3's own zvol file.
	waitFor(t, 20*time.Second, "n3 catches up on the drift writes", func() bool {
		return c.byID["n3"].fileHas(t, volID, pre+drift-1)
	})
	for r := 0; r < pre+drift; r++ {
		if !c.byID["n3"].fileHas(t, volID, r) {
			t.Fatalf("final: record %d missing on resynced n3", r)
		}
	}

	// status.Placement must also end up honest: n3 reported Secondary
	// with the adopted sequence, not a stale row nobody ever revised.
	st := c.Status(volID)
	for _, pl := range st.Placement {
		if pl.NodeID == "n3" {
			if pl.Role != storage.RoleSecondary {
				t.Fatalf("n3 role %v after resync, want Secondary", pl.Role)
			}
			if pl.Sequence < uint64(pre+drift) {
				t.Fatalf("n3 reported sequence %d after resync, want >= %d", pl.Sequence, pre+drift)
			}
		}
	}
}

// TestFailoverAfterOverwrites: the op log records the CRC of each op's
// ORIGINAL payload, but recovery re-reads CURRENT zvol bytes — so any
// overwritten range used to fail every CRC check (leveling NACKs, then
// "unfillable op → volume diverged"). Filesystems overwrite constantly
// (journal, inodes); found by the T21 linearizability suite.
func TestFailoverAfterOverwrites(t *testing.T) {
	const volID = "vol-overwrite"
	c := NewCluster(t, "n1", "n2", "n3")
	c.CreateVolume(volID, 1<<20, []string{"n1", "n2", "n3"})

	waitFor(t, 15*time.Second, "first record acked", func() bool {
		return c.writeAcks(volID, 0) == nil
	})
	// Overwrite record 0 (fully) and record 1 (its second half only).
	if err := c.Write(volID, 0, recData(100)); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 4; i++ {
		if err := c.writeAcks(volID, i); err != nil {
			t.Fatal(err)
		}
	}
	half := recData(101)[recSize/2:]
	if err := c.Write(volID, recSize+recSize/2, half); err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(volID); err != nil {
		t.Fatal(err)
	}

	c.Crash("n1")
	c.Elect(volID, "n2")
	waitFor(t, 30*time.Second, "n2 serves after failover over overwritten ranges", func() bool {
		return c.writeAcks(volID, 8) == nil
	})
	for _, bad := range []string{"leveling failed", "unfillable", "diverged"} {
		if c.LogHas(bad) {
			t.Fatalf("recovery over overwritten ranges logged %q", bad)
		}
	}
	c.Restart("n1")

	waitFor(t, 30*time.Second, "n1 converges with the overwritten state", func() bool {
		return c.byID["n1"].fileHas(t, volID, 8)
	})
	for _, id := range []string{"n1", "n2", "n3"} {
		f, err := os.ReadFile(c.byID[id].zvolFile(volID))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(f[:recSize], recData(100)) {
			t.Fatalf("%s: record 0 is not its overwritten value", id)
		}
		if !bytes.Equal(f[recSize+recSize/2:2*recSize], half) {
			t.Fatalf("%s: record 1's overwritten half is missing", id)
		}
	}
}

// TestDeposedPrimaryBranchNotTrusted: an isolated primary keeps applying
// writes it can never get quorum for. After it heals, a failover must not
// treat that uncommitted branch as evidence ("diverged"): the replica is
// Stale and resyncs from the real primary. Found by T21.
func TestDeposedPrimaryBranchNotTrusted(t *testing.T) {
	const volID = "vol-deposed"
	c := NewFaultCluster(t, "n1", "n2", "n3")
	c.CreateVolume(volID, 1<<20, []string{"n1", "n2", "n3"})
	waitFor(t, 15*time.Second, "first record acked", func() bool {
		return c.writeAcks(volID, 0) == nil
	})
	for i := 1; i < 4; i++ {
		if err := c.writeAcks(volID, i); err != nil {
			t.Fatal(err)
		}
	}

	// Partition n1 (the primary), fail over to n2.
	c.Faults.Isolate(c.byID["n1"].Idx)
	// While its lease is still valid n1 applies ops it can never get quorum for.
	for i := 0; i < 3; i++ {
		_ = c.byID["n1"].RT.WriteOp(volID, int64((20+i)*recSize), recData(500+i))
	}
	c.Elect(volID, "n2")
	waitFor(t, 30*time.Second, "n2 serves", func() bool { return c.writeAcks(volID, 4) == nil })
	waitFor(t, 10*time.Second, "n1 published Stale", func() bool {
		for _, pl := range c.Status(volID).Placement {
			if pl.NodeID == "n1" && pl.Role == storage.RoleStale {
				return true
			}
		}
		return false
	})

	// n1 heals, then n2 dies before n1 has resynced: n3 must take over.
	c.Faults.Heal(c.byID["n1"].Idx)
	c.Crash("n2")
	c.Elect(volID, "n3")
	c.Restart("n2") // back before n3 wins the lease, so n3's recovery probes n1 AND n2
	waitFor(t, 30*time.Second, "n3 serves despite n1's stale branch", func() bool {
		return c.writeAcks(volID, 5) == nil
	})
	for _, bad := range []string{"diverged", "unfillable"} {
		if c.LogHas(bad) {
			t.Fatalf("a Stale replica's uncommitted branch was treated as evidence (%q)", bad)
		}
	}
}

// TestDeposedPrimaryUncommittedWritesDiscarded: the ops an isolated primary
// applied without quorum share sequence numbers with the new primary's
// DIFFERENT ops, so an op-replay resync of the rejoining node (which only
// resends ops past its last seq) never overwrites them — the branch
// survived forever (T21: "n1=1031 n2=994 n3=994").
func TestDeposedPrimaryUncommittedWritesDiscarded(t *testing.T) {
	const volID = "vol-discard"
	c := NewFaultCluster(t, "n1", "n2", "n3")
	c.CreateVolume(volID, 1<<20, []string{"n1", "n2", "n3"})
	waitFor(t, 15*time.Second, "first record acked", func() bool {
		return c.writeAcks(volID, 0) == nil
	})
	for i := 1; i < 4; i++ {
		if err := c.writeAcks(volID, i); err != nil {
			t.Fatal(err)
		}
	}

	c.Faults.Isolate(c.byID["n1"].Idx)
	for i := 0; i < 3; i++ { // applied locally on n1, never quorum-acked
		_ = c.byID["n1"].Runtime().WriteOp(volID, int64((20+i)*recSize), recData(500+i))
	}
	c.Elect(volID, "n2")
	waitFor(t, 30*time.Second, "n2 serves", func() bool { return c.writeAcks(volID, 4) == nil })
	for i := 5; i < 12; i++ { // n2's branch grows past n1's last seq
		if err := c.writeAcks(volID, i); err != nil {
			t.Fatal(err)
		}
	}
	c.Faults.Heal(c.byID["n1"].Idx)

	region := func(id string) []byte {
		f, err := os.ReadFile(c.byID[id].ZvolFile(volID))
		if err != nil {
			return nil
		}
		return f[:30*recSize]
	}
	waitFor(t, 40*time.Second, "n1 discards its uncommitted branch", func() bool {
		want := region("n2")
		return want != nil && bytes.Equal(region("n1"), want) && bytes.Equal(region("n3"), want)
	})
}

// staleElectee builds the scenario for the fetch-drop tests: n2 falls
// behind while n1 and n3 keep acking, then n1 dies. n2 must fill from n3.
func staleElectee(t *testing.T, volID string, cfg Config) (c *Cluster, n2, n3 int) {
	t.Helper()
	ids := []string{"n1", "n2", "n3"}
	cfg.StaleTimeout = 500 * time.Millisecond
	c = NewFaultClusterWith(t, cfg, ids...)
	c.CreateVolume(volID, 1<<20, ids)
	waitFor(t, 15*time.Second, "first record acked", func() bool { return c.writeAcks(volID, 0) == nil })
	n2, n3 = c.byID["n2"].Idx, c.byID["n3"].Idx
	c.Faults.Isolate(n2)
	for i := 1; i < 6; i++ {
		i := i
		waitFor(t, 15*time.Second, fmt.Sprintf("record %d acked by n1+n3", i), func() bool { return c.writeAcks(volID, i) == nil })
	}
	c.Crash("n1")
	c.Faults.Heal(n2)
	return c, n2, n3
}

// TestRecoveryFetchDropIsRetriedNotLatched: the elected node has to fill
// its copy from the one surviving holder, and that holder's link drops as
// the first FetchOps goes out (after its probe already succeeded). That
// says nothing about the holder's data, so recovery must be retried, not
// latched as "claimed durability is missing".
func TestRecoveryFetchDropIsRetriedNotLatched(t *testing.T) {
	const volID = "vol-fetchdrop"
	c, n2, n3 := staleElectee(t, volID, Config{})
	c.Faults.SeverOnFetch(n2, n3)
	c.Elect(volID, "n2")

	waitFor(t, 45*time.Second, "n2 serves after its fetch was dropped once", func() bool { return c.writeAcks(volID, 20) == nil })
	if !c.LogHas("recovery deferred") {
		t.Fatal("the dropped fetch was never seen as transient: the fault did not fire, or recovery took another path")
	}
	if c.LogHas("manual recovery required") || c.Status(volID).State == storage.StateNeedsManualRecovery {
		t.Fatal("a dropped connection latched the volume for manual recovery")
	}
	for i := 0; i < 6; i++ {
		i := i
		waitFor(t, 15*time.Second, fmt.Sprintf("acked record %d present on n2", i), func() bool { return c.byID["n2"].fileHas(t, volID, i) })
	}
}

// TestPersistentFetchDropsStillLatch: a holder that answers probes but
// never manages to serve is indistinguishable from a dishonest one, so
// transport failures that persist past the window still stop for an
// operator (the original §4.3 4b safeguard).
func TestPersistentFetchDropsStillLatch(t *testing.T) {
	const volID = "vol-fetchflap"
	c, n2, n3 := staleElectee(t, volID, Config{TransientRecoveryWindow: 1500 * time.Millisecond})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { // every attempt's first fetch dies
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				c.Faults.SeverOnFetch(n2, n3)
			}
		}
	}()
	defer func() { close(stop); <-done }()
	c.Elect(volID, "n2")

	waitFor(t, 45*time.Second, "persistent transport failures latch the volume", func() bool {
		return c.Status(volID).State == storage.StateNeedsManualRecovery
	})
	if !c.LogHas("recovery deferred") {
		t.Fatal("expected some retries before the window ran out")
	}
}

// TestSoleCurrentResyncsStale (name kept short: the NBD socket path embeds it): the primary dies while
// one replica is Stale and the survivor is the only current copy. The volume
// must not go offline (that survivor holds every acked write), and the Stale
// peer must be brought back by itself, restoring full redundancy.
func TestSoleCurrentResyncsStale(t *testing.T) {
	const volID = "vol-sole"
	ids := []string{"n1", "n2", "n3"}
	c := NewFaultClusterWith(t, Config{StaleTimeout: 500 * time.Millisecond}, ids...)
	c.CreateVolume(volID, 1<<20, ids)
	waitFor(t, 15*time.Second, "first record acked", func() bool { return c.writeAcks(volID, 0) == nil })

	// n2 misses acked writes and is published Stale; n1+n3 keep acking.
	c.Faults.Isolate(c.byID["n2"].Idx)
	for i := 1; i < 6; i++ {
		i := i
		waitFor(t, 15*time.Second, fmt.Sprintf("record %d acked by n1+n3", i), func() bool { return c.writeAcks(volID, i) == nil })
	}
	waitFor(t, 15*time.Second, "n2 published Stale", func() bool { return c.roleOf(volID, "n2") == storage.RoleStale })

	// The primary dies; n2 is reachable again (still Stale), n3 is the only current copy.
	c.Crash("n1")
	c.Faults.Heal(c.byID["n2"].Idx)
	c.Elect(volID, "n3")

	// Writes need a second copy, so they succeed only once n2 was resynced by itself.
	waitFor(t, 45*time.Second, "n3 serves and n2 rejoins", func() bool { return c.writeAcks(volID, 20) == nil })
	if !c.LogHas("sole current replica") {
		t.Fatal("the survivor never took the sole-current path")
	}
	waitFor(t, 30*time.Second, "n2 is a current secondary again", func() bool { return c.roleOf(volID, "n2") == storage.RoleSecondary })
	for _, i := range []int{0, 1, 5, 20} {
		i := i
		waitFor(t, 15*time.Second, fmt.Sprintf("record %d on n2", i), func() bool { return c.byID["n2"].fileHas(t, volID, i) })
	}
}

// TestTornReplicaRebuilt: a replica whose oplog survived a crash but whose
// zvol lost the bytes must not be declared current on its oplog alone, whether it
// missed a few ops while down ("behind") or is the old primary rejoining after a
// failover ("failover", the vol-durability shape; the harness rebuilds it by another route too).
func TestTornReplicaRebuilt(t *testing.T) {
	t.Run("behind", func(t *testing.T) { tornReplicaIsRebuilt(t, "vol-tb", "n3", false) })
	t.Run("failover", func(t *testing.T) { tornReplicaIsRebuilt(t, "vol-tf", "n1", true) })
}

func tornReplicaIsRebuilt(t *testing.T, volID, victim string, failover bool) {
	const recs = 4
	c := NewCluster(t, "n1", "n2", "n3")
	c.CreateVolume(volID, 1<<20, []string{"n1", "n2", "n3"})
	waitFor(t, 15*time.Second, "first record acked", func() bool { return c.writeAcks(volID, 0) == nil })
	for i := 1; i < recs; i++ {
		if err := c.writeAcks(volID, i); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 15*time.Second, victim+" has every record", func() bool { return c.byID[victim].fileHas(t, volID, recs-1) })

	c.Crash(victim)
	zvol := c.byID[victim].zvolFile(volID) // same size, all zeros: the bytes never reached disk
	if err := os.Truncate(zvol, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(zvol, 1<<20); err != nil {
		t.Fatal(err)
	}
	last := recs - 1
	if failover {
		c.Elect(volID, "n2")
	} else {
		last = recs
		if err := c.writeAcks(volID, recs); err != nil { // the survivors keep acking
			t.Fatal(err)
		}
	}
	c.Restart(victim)

	for i := 0; i <= last; i++ {
		i := i
		waitFor(t, 60*time.Second, fmt.Sprintf("record %d rebuilt on %s", i, victim), func() bool { return c.byID[victim].fileHas(t, volID, i) })
	}
}

// TestFencedPrimaryServesAgainPromptly: a real double failure fences the primary
// (its writes lose quorum). Once the secondaries are back the volume must serve
// again within a few lease terms, not minutes.
func TestFencedPrimaryServesAgainPromptly(t *testing.T) {
	const volID = "vol-fence"
	c := NewCluster(t, "n1", "n2", "n3")
	c.CreateVolume(volID, 1<<20, []string{"n1", "n2", "n3"})
	waitFor(t, 15*time.Second, "first record acked", func() bool { return c.writeAcks(volID, 0) == nil })

	c.Crash("n2")
	c.Crash("n3")
	if err := c.writeAcks(volID, 1); err == nil {
		t.Fatal("a write with both secondaries down must not be acked")
	}
	c.Restart("n2")
	c.Restart("n3")

	start := time.Now()
	waitFor(t, 30*time.Second, "primary serves again", func() bool { return c.writeAcks(volID, 2) == nil })
	t.Logf("served again %v after the secondaries returned", time.Since(start).Round(100*time.Millisecond))
	for _, id := range []string{"n1", "n2", "n3"} {
		id := id
		waitFor(t, 30*time.Second, "record 2 on "+id, func() bool { return c.byID[id].fileHas(t, volID, 2) })
	}
}

// TestDemotedPrimaryReportsWhatItWrote: a node that led after following holds its
// own writes in its durable oplog, so once it steps down its probe must say so (a
// stale answer makes recovery look for ops that no replica's claims can supply).
func TestDemotedPrimaryReportsWhatItWrote(t *testing.T) {
	const volID = "vol-demoted"
	ids := []string{"n1", "n2", "n3"}
	c := NewCluster(t, ids...)
	c.CreateVolume(volID, 1<<20, ids)
	waitFor(t, 15*time.Second, "first record acked", func() bool { return c.writeAcks(volID, 0) == nil })

	c.Elect(volID, "n2") // n2 was a secondary of n1 and now leads
	waitFor(t, 20*time.Second, "n2 serves", func() bool { return c.writeAcks(volID, 1) == nil })
	for i := 2; i < 6; i++ {
		if err := c.writeAcks(volID, i); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 15*time.Second, "n1 has every write", func() bool { return c.byID["n1"].fileHas(t, volID, 5) })
	wrote, err := c.ProbeSeq(volID, "n1")
	if err != nil {
		t.Fatal(err)
	}

	c.Elect(volID, "n3")
	waitFor(t, 20*time.Second, "n2 demoted", func() bool { return !c.byID["n2"].DeviceAttached() })

	got, err := c.ProbeSeq(volID, "n2")
	if err != nil {
		t.Fatal(err)
	}
	if got < wrote {
		t.Fatalf("demoted primary reports seq %d, but it wrote through %d", got, wrote)
	}
}

func (c *Cluster) roleOf(volID, node string) storage.Role {
	for _, pl := range c.Status(volID).Placement {
		if pl.NodeID == node {
			return pl.Role
		}
	}
	return ""
}
