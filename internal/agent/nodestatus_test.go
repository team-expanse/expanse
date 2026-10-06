package agent

import (
	"testing"

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
