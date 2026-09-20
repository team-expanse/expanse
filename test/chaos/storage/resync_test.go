package chaosstorage

import (
	"testing"
	"time"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/test/chaos/exvol"
)

const (
	resyncRate   = 8 << 20 // bytes/s cap on resync streams
	resyncBudget = 90 * time.Second
	latencyFloor = 250 * time.Millisecond // p99 allowance however quiet the baseline was
)

// TestResyncUnderLoad rejoins a replica while clients keep writing. The
// resync is rate limited and must neither fail nor slow foreground writes.
func TestResyncUnderLoad(t *testing.T) {
	t.Run("op-replay", func(t *testing.T) { runResyncUnderLoad(t, false, 20*time.Millisecond) })
	t.Run("snapshot", func(t *testing.T) { runResyncUnderLoad(t, true, 20*time.Millisecond) })
	t.Run("saturating", func(t *testing.T) { runResyncUnderLoad(t, false, 0) })
}

// runResyncUnderLoad crashes one secondary under load, brings it back
// (with its pool wiped if wipe) and times the resync it triggers.
func runResyncUnderLoad(t *testing.T, wipe bool, think time.Duration) {
	const victim = "n3"
	c := newVolume(t, exvol.Config{ResyncBytesPerSec: resyncRate, OpReplayMaxOps: 4096})
	l := NewLoad(c, vol)
	stopLoad := l.Run(6, think)
	defer stopLoad()

	time.Sleep(4 * time.Second)
	baseline := l.Samples()
	c.Crash(victim)
	if wipe {
		c.WipePool(victim)
	}
	time.Sleep(3 * time.Second)
	t0 := l.Since()
	c.Restart(victim)
	took := waitResynced(t, c, l, victim, t0)
	during := l.Samples().Window(t0, l.Since())
	t.Log(describe("baseline", baseline))
	t.Logf("resync took %v", took)
	t.Log(describe("during", during))

	if errs := during.Errors(); len(errs) > 0 {
		t.Errorf("resync failed %d foreground writes, first: %v", len(errs), errs[0].Err)
	}
	if bound := max(20*baseline.Percentile(0.99), latencyFloor); during.Percentile(0.99) > bound {
		t.Errorf("foreground p99 %v during resync exceeds %v (baseline p99 %v)", during.Percentile(0.99), bound, baseline.Percentile(0.99))
	}
	if c.LogHas("FULL send") {
		if floor := fullSendFloor(); took < floor {
			t.Errorf("full resync of %d MiB took %v, under the %v the %d MiB/s cap allows: rate limit not applied",
				volSize>>20, took, floor, resyncRate>>20)
		}
	}
	stopLoad()
	assertNoLoss(t, c, l)
}

// waitResynced waits until victim is a current secondary again and
// returns how long that took from t0 (run clock).
func waitResynced(t *testing.T, c *exvol.Cluster, l *Load, victim string, t0 time.Duration) time.Duration {
	t.Helper()
	for role(c, victim) != storage.RoleSecondary || unhealthy(c) != "" {
		if l.Since()-t0 > resyncBudget {
			t.Fatalf("resync not done after %v: %s (%+v)", resyncBudget, unhealthy(c), c.Status(vol).Placement)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return l.Since() - t0
}

// fullSendFloor is half the time a token bucket needs to move the whole
// volume past its burst allowance (half is slack for send granularity).
func fullSendFloor() time.Duration {
	const burst = resyncRate / 2
	return time.Duration(float64(volSize-burst) / resyncRate / 2 * float64(time.Second))
}
