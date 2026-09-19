package primary

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/storage/exvol/oplog"
	"github.com/expanse/expanse/internal/storage/exvol/protocol"
)

// staleWithMissedOps: r2 holds ops 1-2, is then marked Stale, and misses 3-5.
func staleWithMissedOps(t *testing.T) (*Coordinator, *testReplica) {
	t.Helper()
	r1, r2 := newTestReplica(t, "r1", 1<<16), newTestReplica(t, "r2", 1<<16)
	c, _, _ := newTestPrimary(t, 1<<16, []*testReplica{r1, r2}, nil, 500*time.Millisecond)
	c.AttachOplog(oplog.NewMemory())
	write := func(i int) {
		if err := c.Write(bytes.Repeat([]byte{byte(i)}, 8), int64(i*8)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	write(1)
	write(2)
	waitFor(t, func() bool { return r2.lastSeq() == 2 }, "r2 never reached seq 2")
	c.MarkStale("r2")
	for i := 3; i <= 5; i++ {
		write(i)
	}
	return c, r2
}

func TestReviveCaughtUpReplaysMissedOpsBeforeAdmitting(t *testing.T) {
	c, r2 := staleWithMissedOps(t)
	var sent []uint64
	deliver := func(op protocol.WriteOp) error {
		sent = append(sent, op.Seq)
		if rep := r2.handle(op); !rep.ACK {
			t.Fatalf("op %d nacked: %s", op.Seq, rep.Reason)
		}
		return nil
	}
	if err := c.ReviveCaughtUp("r2", r2.connect(t), 2, deliver); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 || sent[0] != 3 || sent[2] != 5 {
		t.Fatalf("replayed %v, want [3 4 5]", sent)
	}
	if got := c.StaleReplicas(); len(got) != 0 {
		t.Fatalf("still stale after revive: %v", got)
	}
	// A live write after the revive lands with no sequence gap.
	if err := c.Write(bytes.Repeat([]byte{9}, 8), 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return r2.lastSeq() == 6 }, "r2 missed the post-revive op")
}

func TestReviveCaughtUpFailureKeepsReplicaStale(t *testing.T) {
	c, r2 := staleWithMissedOps(t)
	boom := errors.New("target died")
	if err := c.ReviveCaughtUp("r2", r2.connect(t), 2, func(protocol.WriteOp) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the delivery error", err)
	}
	if got := c.StaleReplicas(); len(got) != 1 || got[0] != "r2" {
		t.Fatalf("stale = %v, want [r2]: a failed catch-up must not admit the replica", got)
	}
}

// An op whose range a later op rewrote can only be replayed as the
// range's CURRENT bytes; its CRC must describe them or every replica
// NACKs the replay (the T21 overwrite bug).
func TestFetchOpsOverwrittenRangeCarriesValidCRC(t *testing.T) {
	c, _, _ := newTestPrimary(t, 1<<16, nil, nil, 200*time.Millisecond)
	c.AttachOplog(oplog.NewMemory())
	for _, b := range []byte{1, 2} { // same range, twice
		if err := c.Write(bytes.Repeat([]byte{b}, 4096), 0); err != nil {
			t.Fatal(err)
		}
	}
	ops, err := c.FetchOps(0, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if !op.ValidCRC() || !bytes.Equal(op.Data, bytes.Repeat([]byte{2}, 4096)) {
			t.Fatalf("op %d: want current bytes under a valid CRC, got crc-valid=%v data[0]=%d", op.Seq, op.ValidCRC(), op.Data[0])
		}
	}
}
