package primary

import (
	"testing"
	"time"
)

// A secondary that is slow but inside the stale timeout must not pace
// the volume: quorum is local + the fast secondary (§9).
func TestSlowSecondaryBelowTimeoutDoesNotThrottleWrites(t *testing.T) {
	const writes = 40
	size := int64(1 << 20)
	fast := newTestReplica(t, "n1", size)
	slow := newTestReplica(t, "n2", size)
	slow.mu.Lock()
	slow.delay = 40 * time.Millisecond
	slow.mu.Unlock()
	c, _, _ := newTestPrimary(t, size, []*testReplica{fast, slow}, &fakeLease{valid: true}, 5*time.Second)

	start := time.Now()
	for i := 0; i < writes; i++ {
		if err := c.Write([]byte("x"), int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	// Paced by the slow replica this takes writes*40ms = 1.6 s.
	if took := time.Since(start); took > 800*time.Millisecond {
		t.Errorf("%d writes took %v: a slow secondary is pacing the volume", writes, took)
	}
	if st := c.StaleReplicas(); len(st) != 0 {
		t.Errorf("replica inside the stale timeout was marked Stale: %v", st)
	}
	deadline := time.Now().Add(5 * time.Second)
	for slow.lastSeq() < writes && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := slow.lastSeq(); got != writes {
		t.Errorf("slow secondary reached seq %d, want %d: it must still receive every op", got, writes)
	}
}

// An ack for op N proves nothing about op N+1: a slow replica's late
// reply to the previous write must not complete the next write's quorum.
func TestLateAckForOlderOpDoesNotSatisfyNewerWrite(t *testing.T) {
	size := int64(1 << 20)
	a := newTestReplica(t, "n1", size)
	b := newTestReplica(t, "n2", size)
	a.delayFor = func(seq uint64) time.Duration { return map[uint64]time.Duration{2: time.Second}[seq] }
	b.delayFor = func(seq uint64) time.Duration {
		return map[uint64]time.Duration{1: 200 * time.Millisecond, 2: 3 * time.Second}[seq]
	}
	c, _, _ := newTestPrimary(t, size, []*testReplica{a, b}, &fakeLease{valid: true}, 5*time.Second)

	if err := c.Write([]byte("one"), 0); err != nil { // acked by n1; n2's reply is 200 ms late
		t.Fatal(err)
	}
	if err := c.Write([]byte("two"), 10); err != nil {
		t.Fatal(err)
	}
	if a.lastSeq() < 2 && b.lastSeq() < 2 {
		t.Fatalf("write 2 acked with no secondary holding it (n1 at %d, n2 at %d): a late ack for write 1 was counted", a.lastSeq(), b.lastSeq())
	}
}

// A replica that stops acking must go Stale on the reconcile tick even
// if the volume is idle afterwards (no later write to notice it).
func TestUnackedReplicaExpiresOnIdleVolume(t *testing.T) {
	size := int64(1 << 20)
	fast := newTestReplica(t, "n1", size)
	wedged := newTestReplica(t, "n2", size)
	wedged.mu.Lock()
	wedged.delay = 5 * time.Second
	wedged.mu.Unlock()
	c, _, _ := newTestPrimary(t, size, []*testReplica{fast, wedged}, &fakeLease{valid: true}, 300*time.Millisecond)

	if err := c.Write([]byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	if st := c.StaleReplicas(); len(st) != 0 {
		t.Fatalf("Stale before the timeout elapsed: %v", st)
	}
	time.Sleep(450 * time.Millisecond)
	c.DrainResults()
	if st := c.StaleReplicas(); len(st) != 1 || st[0] != "n2" {
		t.Fatalf("StaleReplicas = %v, want [n2]", st)
	}
}
