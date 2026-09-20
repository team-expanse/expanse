// Phase 06 T23 storage budgets. They are measured inside the vol-perf
// VM test (nix/tests/vol-perf.nix) against a real ZFS pool, so this
// package only pins their definitions and keeps CheckAll from
// reporting them as measured here.
package perf

import (
	"strings"
	"testing"
)

func budgetByName(t *testing.T, name string) Budget {
	t.Helper()
	for _, b := range LoadBudgets() {
		if b.Name == name {
			return b
		}
	}
	t.Fatalf("budget %s missing from budgets.yaml", name)
	return Budget{}
}

func TestVolumeBudgetsPinTheSpecFloors(t *testing.T) {
	floors := map[string]float64{
		"exvol_seqwrite_ratio":  0.75,
		"exvol_seqread_ratio":   0.95,
		"exvol_randwrite_ratio": 0.60,
		"exvol_randread_ratio":  0.95,
	}
	for name, min := range floors {
		if got := budgetByName(t, name); got.Min != min || got.Max != 0 {
			t.Errorf("%s: min=%v max=%v, want a floor of %v", name, got.Min, got.Max, min)
		}
	}
}

func TestVolumeBudgetsPinTheCeilings(t *testing.T) {
	if got := budgetByName(t, "vol_failover_ms"); got.Max != 20000 {
		t.Errorf("vol_failover_ms max=%v, want 20000 (G6.4)", got.Max)
	}
	if got := budgetByName(t, "exvol_fsync_p99_us"); got.Max <= 0 {
		t.Errorf("exvol_fsync_p99_us needs an absolute ceiling, got %v", got.Max)
	}
}

func TestVolumeResyncRateIsAFloorFromG67(t *testing.T) {
	// G6.7: 100 MiB of drift within 60 s is at least 100/60 MiB/s.
	got := budgetByName(t, "vol_resync_mbps")
	if got.Min < 100.0/60.0 || got.Max != 0 {
		t.Errorf("vol_resync_mbps min=%v max=%v, want a floor of at least 100/60", got.Min, got.Max)
	}
}

func TestVolumeBudgetsAreLeftUnmeasuredHere(t *testing.T) {
	var vol []Budget
	for _, b := range LoadBudgets() {
		if strings.HasPrefix(b.Name, "exvol_") || strings.HasPrefix(b.Name, "vol_") {
			vol = append(vol, b)
		}
	}
	if len(vol) != 7 {
		t.Fatalf("want 7 storage budgets, found %d", len(vol))
	}
	if err := CheckAll(vol); err != nil {
		t.Fatalf("unmeasured budgets must never be violations: %v", err)
	}
	for _, b := range vol {
		if b.Measured >= 0 {
			t.Errorf("%s reports %v; storage budgets are VM-only", b.Name, b.Measured)
		}
	}
}
