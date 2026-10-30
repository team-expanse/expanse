package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/agent/health"
)

func TestNodeStatusValue(t *testing.T) {
	rep := &health.Report{Overall: health.Unhealthy, Checks: []health.Result{
		{Name: "disk-space", Status: health.Healthy}, {Name: "clock-sync", Status: health.Unhealthy},
	}}
	if got, want := nodeStatusValue(rep, false), "health=unhealthy schedulable=true"; got != want {
		t.Errorf("nodeStatusValue = %q, want %q", got, want)
	}
	if got, want := nodeStatusValue(rep, true), "health=unhealthy schedulable=true degraded=true writable=false"; got != want {
		t.Errorf("nodeStatusValue (no quorum) = %q, want %q", got, want)
	}
}

// A restarted node's first status goes out as soon as the store takes it, not a full period later:
// until it does, the leader still counts the node as down and its replicas' grace runs out.
func TestFirstSuccessRetriesUntilTheWriteLands(t *testing.T) {
	calls := 0
	firstSuccess(context.Background(), time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return errors.New("no leader yet")
		}
		return nil
	})
	if calls != 3 {
		t.Errorf("calls = %d, want 3: tried at once, retried, stopped after the first success", calls)
	}
}

func TestFirstSuccessStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	firstSuccess(ctx, time.Millisecond, func() error {
		if calls++; calls == 2 {
			cancel()
		}
		return errors.New("no leader yet")
	})
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}
