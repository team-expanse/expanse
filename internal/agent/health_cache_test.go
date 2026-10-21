package agent

import (
	"context"
	"testing"

	"github.com/expanse/expanse/internal/agent/health"
)

type countingCheck struct{ runs int }

func (c *countingCheck) Name() string { return "counting" }
func (c *countingCheck) Run(context.Context) health.Result {
	c.runs++
	return health.Result{Name: c.Name(), Status: health.Healthy}
}

// Reads must not re-run the checks: the store check writes a key the web UI
// watches, so each read would trigger the next.
func TestHealthReadsServeTheLatestRun(t *testing.T) {
	check := &countingCheck{}
	a := &Agent{healthR: &health.Runner{Checks: []health.Check{check}}}
	ctx := context.Background()

	first := a.Health(ctx)
	if a.Health(ctx) != first || check.runs != 1 {
		t.Fatalf("runs = %d after two reads, want 1 (the first read fills the cache)", check.runs)
	}
	ran := a.runHealth(ctx)
	if a.Health(ctx) != ran || check.runs != 2 {
		t.Errorf("reads do not serve the health loop's latest report (runs = %d)", check.runs)
	}
}
