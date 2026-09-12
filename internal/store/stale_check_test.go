package store

import (
	"os"
	"testing"
)

// TestNoStaleReadsInSensitivePackages enforces that scheduling and lease
// code never opts into stale reads (Phase 03 §10, "Followers serving
// stale reads accidentally"). Packages that do not exist yet are skipped.
func TestNoStaleReadsInSensitivePackages(t *testing.T) {
	sensitive := []string{
		"../../scheduler",
		"../../cluster/lease",
	}
	for _, dir := range sensitive {
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		if bad := AssertNoStaleReads(dir); len(bad) > 0 {
			t.Errorf("forbidden stale reads in %s: %v", dir, bad)
		}
	}
}
