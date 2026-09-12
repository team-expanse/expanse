package perf

import (
	"os"
	"testing"
)

// TestBudgets checks performance budgets. Skipped unless RUN_PERF=1,
// because it requires a freshly built bin/expanse.
func TestBudgets(t *testing.T) {
	if os.Getenv("RUN_PERF") == "" {
		t.Skip("set RUN_PERF=1 to run performance budget checks")
	}
	budgets := LoadBudgets()
	if err := CheckAll(budgets); err != nil {
		t.Fatal(err)
	}
	for _, b := range budgets {
		t.Logf("%s: %.0f %s (budget %.0f %s)", b.Name, b.Measured, b.Unit, b.Max, b.Unit)
	}
}
