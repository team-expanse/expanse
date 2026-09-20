package chaosstorage

import (
	"testing"
	"time"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/test/chaos/exvol"
)

const (
	slowStaleTimeout = time.Second
	slowObserve      = 10 * time.Second
	slowMaxStall     = time.Second
)

// TestSlowSecondary delays one secondary's links. A slow replica may be
// marked Stale (§9) but must never slow or fail client writes: quorum is
// the primary plus the healthy secondary.
func TestSlowSecondary(t *testing.T) {
	cases := []struct {
		name      string
		delay     time.Duration // per message, on the victim's links
		writers   int
		think     time.Duration
		wantStale bool
	}{
		// ~100 ops/s is well inside what a 200 ms link carries: slow, not failing.
		{"keeps-up-within-timeout", 200 * time.Millisecond, 2, 100 * time.Millisecond, false},
		// Replies take 3 s against a 1 s timeout: the replica is unresponsive.
		{"unresponsive-beyond-timeout", 1500 * time.Millisecond, 6, 20 * time.Millisecond, true},
		// ~2 MB/s of writes into a link that carries ~0.3 MB/s: falls ever further behind.
		{"cannot-keep-up", 200 * time.Millisecond, 6, 20 * time.Millisecond, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runSlowSecondary(t, tc.delay, tc.writers, tc.think, tc.wantStale)
		})
	}
}

func runSlowSecondary(t *testing.T, delay time.Duration, writers int, think time.Duration, wantStale bool) {
	const victim = "n3"
	c := newVolume(t, exvol.Config{StaleTimeout: slowStaleTimeout})
	l := NewLoad(c, vol)
	stopLoad := l.Run(writers, think)
	defer stopLoad()

	time.Sleep(3 * time.Second)
	baseline := l.Samples()
	t0 := l.Since()
	c.Faults.SetDelay(c.Node(victim).Idx, delay)
	staleAfter := watchStale(c, l, victim, t0)
	fault := l.Samples().Window(t0, l.Since())
	t.Log(describe("baseline", baseline))
	t.Log(describe("fault   ", fault), "stale after:", staleAfter)

	if errs := fault.Errors(); len(errs) > 0 {
		t.Errorf("a slow secondary failed %d client writes, first: %v", len(errs), errs[0].Err)
	}
	if bound := max(20*baseline.Percentile(0.99), latencyFloor); fault.Percentile(0.99) > bound {
		t.Errorf("client p99 %v with a slow secondary exceeds %v (baseline p99 %v)", fault.Percentile(0.99), bound, baseline.Percentile(0.99))
	}
	if got := fault.LongestStall(); got > slowMaxStall {
		t.Errorf("volume made no progress for %v with a slow secondary (limit %v)", got, slowMaxStall)
	}
	switch {
	case wantStale && staleAfter < 0:
		t.Errorf("secondary never marked Stale within %v", slowObserve)
	case wantStale && staleAfter > 5*slowStaleTimeout:
		t.Errorf("secondary marked Stale after %v, want within ~%v", staleAfter, slowStaleTimeout)
	case !wantStale && staleAfter >= 0:
		t.Errorf("secondary marked Stale after %v though it kept up within the timeout", staleAfter)
	}

	c.Faults.SetDelay(c.Node(victim).Idx, 0)
	waitHealthy(t, c, resyncBudget)
	stopLoad()
	assertNoLoss(t, c, l)
}

// watchStale polls for slowObserve and returns how long after t0 victim
// was first out of the quorum (Stale, or already Resyncing: the runtime
// moves a Stale replica straight to Resync retries), or -1 if never.
func watchStale(c *exvol.Cluster, l *Load, victim string, t0 time.Duration) time.Duration {
	after := time.Duration(-1)
	for l.Since()-t0 < slowObserve {
		if r := role(c, victim); after < 0 && (r == storage.RoleStale || r == storage.RoleResyncing) {
			after = l.Since() - t0
		}
		time.Sleep(50 * time.Millisecond)
	}
	return after
}
