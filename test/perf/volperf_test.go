// Phase 06 T23 storage budgets. They are measured inside the vol-perf
// VM test (nix/tests/vol-perf.nix) against a real LVM volume group, so this
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
		"vol_seqwrite_ratio":  0.75,
		"vol_seqread_ratio":   0.95,
		"vol_randwrite_ratio": 0.60,
		"vol_randread_ratio":  0.95,
	}
	for name, min := range floors {
		if got := budgetByName(t, name); got.Min != min || got.Max != 0 {
			t.Errorf("%s: min=%v max=%v, want a floor of %v", name, got.Min, got.Max, min)
		}
	}
}

// The write ratios and the failover time of the spec cannot be judged in the two-vCPU VM harness (E5):
// each carries a vm_waiver naming why, and the gates below stand in for what the software controls.
func TestVolumeBudgetsBeyondTheSpecAreFloorsAndCeilings(t *testing.T) {
	floors := map[string]float64{
		"vol_r1_seqwrite_ratio":        0.90,
		"vol_r1_randwrite_ratio":       0.80,
		"vol_r3_seqwrite_of_r2_ratio":  0.45,
		"vol_r3_randwrite_of_r2_ratio": 0.55,
		"vol_first_touch_ratio":        0.70,
	}
	for name, min := range floors {
		if got := budgetByName(t, name); got.Min != min || got.Max != 0 {
			t.Errorf("%s: min=%v max=%v, want a floor of %v", name, got.Min, got.Max, min)
		}
	}
	if got := budgetByName(t, "vol_failover_worst_ms"); got.Max != 30000 || got.Min != 0 {
		t.Errorf("vol_failover_worst_ms min=%v max=%v, want a ceiling of 30000", got.Min, got.Max)
	}
}

func TestOnlyTheSpecBudgetsTheVMCannotJudgeAreWaived(t *testing.T) {
	waived := map[string]bool{"vol_seqwrite_ratio": true, "vol_seqread_ratio": true, "vol_randwrite_ratio": true, "vol_failover_ms": true}
	for _, b := range LoadBudgets() {
		if strings.HasPrefix(b.Name, "vol_") && (b.VMWaiver != "") != waived[b.Name] {
			t.Errorf("%s: waiver %q, want a waiver exactly on %v", b.Name, b.VMWaiver, waived)
		}
	}
}

func TestVolumeBudgetsPinTheCeilings(t *testing.T) {
	if got := budgetByName(t, "vol_failover_ms"); got.Max != 20000 {
		t.Errorf("vol_failover_ms max=%v, want 20000 (G6.4)", got.Max)
	}
	if got := budgetByName(t, "vol_fsync_p99_us"); got.Max <= 0 {
		t.Errorf("vol_fsync_p99_us needs an absolute ceiling, got %v", got.Max)
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
		if strings.HasPrefix(b.Name, "vol_") {
			vol = append(vol, b)
		}
	}
	if len(vol) != 13 {
		t.Fatalf("want 13 storage budgets, found %d", len(vol))
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
