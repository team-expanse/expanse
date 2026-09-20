package chaosstorage

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/test/chaos/exvol"
	"github.com/expanse/expanse/test/chaos/storage/linearizability"
)

const (
	vol     = "vol-chaos"
	volSize = 64 << 20
)

var nodeIDs = []string{"n1", "n2", "n3"}

// scenarioDuration: RUN_CHAOS=1 → 10 minutes (CI nightly), CHAOS_DURATION
// overrides both, otherwise the compressed local default.
func scenarioDuration(local time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv("CHAOS_DURATION")); err == nil && d > 0 {
		return d
	}
	if os.Getenv("RUN_CHAOS") == "1" {
		return 10 * time.Minute
	}
	return local
}

func newVolume(t *testing.T, cfg exvol.Config) *exvol.Cluster {
	t.Helper()
	c := exvol.NewFaultClusterWith(t, cfg, nodeIDs...)
	c.CreateVolume(vol, volSize, nodeIDs)
	waitPrimaryServes(t, c)
	return c
}

// waitPrimaryServes waits for initial promotion so a run starts serving.
func waitPrimaryServes(t *testing.T, c *exvol.Cluster) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if c.Write(vol, 0, make([]byte, blockSize)) == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("volume primary never came up")
}

// waitHealthy waits for every replica to be current and out of Stale/Resyncing.
func waitHealthy(t *testing.T, c *exvol.Cluster, budget time.Duration) time.Duration {
	t.Helper()
	t0 := time.Now()
	var why string
	for time.Since(t0) < budget {
		if why = linearizability.Unhealthy(c, vol, nodeIDs); why == "" {
			return time.Since(t0)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("volume not healthy after %v: %s (placement %+v)", budget, why, c.Status(vol).Placement)
	return 0
}

func role(c *exvol.Cluster, node string) storage.Role {
	for _, pl := range c.Status(vol).Placement {
		if pl.NodeID == node {
			return pl.Role
		}
	}
	return ""
}

// assertNoLoss is the shared durability verdict once a scenario has
// healed: nothing read back was wrong, every acked write is still there,
// and all replicas hold identical bytes.
func assertNoLoss(t *testing.T, c *exvol.Cluster, l *Load) {
	t.Helper()
	for _, v := range l.Violations() {
		t.Errorf("read-back after ack: %s", v)
	}
	for _, err := range l.VerifyDurable(60 * time.Second) {
		t.Errorf("durability: %v", err)
	}
	blocks := l.Ledger.Blocks()
	if len(blocks) == 0 {
		t.Fatal("no writes were sent: the run proved nothing")
	}
	end := int64(blocks[len(blocks)-1]+1) * blockSize
	deadline := time.Now().Add(60 * time.Second)
	diff := c.FirstDivergence(vol, end)
	for diff != "" && time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		diff = c.FirstDivergence(vol, end)
	}
	if diff != "" {
		t.Errorf("replicas never converged: %s", diff)
	}
}

func describe(name string, s Samples) string {
	return fmt.Sprintf("%s: n=%d err=%d p50=%v p99=%v max=%v longest-stall=%v",
		name, len(s), len(s.Errors()), s.Percentile(0.5), s.Percentile(0.99), s.Percentile(1), s.LongestStall())
}

func unhealthy(c *exvol.Cluster) string {
	return linearizability.Unhealthy(c, vol, nodeIDs)
}
