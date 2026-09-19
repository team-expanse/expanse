package primary

import (
	"bytes"
	"testing"
	"time"
)

// A read must never observe a write that is not yet quorum-durable: the
// primary applies locally BEFORE fan-out, and a write that then fails to
// reach quorum can be lost at failover — a read that already returned it
// would see the value vanish (T21 linearizability violation).
func TestReadBarrierWaitsForOverlappingInFlightWrite(t *testing.T) {
	r1, r2 := newTestReplica(t, "r1", 1<<16), newTestReplica(t, "r2", 1<<16)
	r1.delay, r2.delay = 300*time.Millisecond, 300*time.Millisecond
	c, _, _ := newTestPrimary(t, 1<<16, []*testReplica{r1, r2}, nil, 2*time.Second)

	done := make(chan error, 1)
	go func() { done <- c.Write(bytes.Repeat([]byte{7}, 4096), 0) }()
	time.Sleep(50 * time.Millisecond) // the write is now applied locally, awaiting acks

	start := time.Now()
	if err := c.ReadBarrier(8192, 4096); err != nil { // disjoint range
		t.Fatal(err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("a read of untouched bytes waited %v behind an unrelated write", d)
	}
	if err := c.ReadBarrier(0, 4096); err != nil { // overlapping range
		t.Fatal(err)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("a read of in-flight bytes returned after %v, before the write was durable", d)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestQuorumFailureFencesPrimary(t *testing.T) {
	r1, r2 := newTestReplica(t, "r1", 1<<16), newTestReplica(t, "r2", 1<<16)
	r1.delay, r2.delay = 2*time.Second, 2*time.Second // both miss the 150ms stale deadline
	c, _, _ := newTestPrimary(t, 1<<16, []*testReplica{r1, r2}, nil, 150*time.Millisecond)

	if err := c.Write(bytes.Repeat([]byte{7}, 4096), 0); err == nil {
		t.Fatal("write with no replica acks must fail")
	}
	if !c.Fenced() {
		t.Fatal("a primary holding an un-replicated local write must fence itself")
	}
	if err := c.ReadBarrier(0, 4096); err == nil {
		t.Fatal("reads must fail once fenced: the local bytes were never committed")
	}
	if err := c.Write(bytes.Repeat([]byte{8}, 4096), 4096); err == nil {
		t.Fatal("writes must fail once fenced")
	}
}
