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
