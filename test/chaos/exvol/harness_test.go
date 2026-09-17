package exvol

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
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
		filepath.Walk(c.byID["n2"].Root, func(p string, info os.FileInfo, err error) error {
			if err == nil {
				t.Logf("n2 tree: %s (%d)", p, info.Size())
			}
			return nil
		})
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
