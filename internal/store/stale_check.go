package store

import (
	"os"
	"path/filepath"
	"strings"
)

// AssertNoStaleReads scans Go files under dir for WithStale usages and
// returns a description of every violation. Sensitive packages (scheduling,
// leases) must never opt into stale reads: a lease decision made on stale
// state is how split-brain happens.
//
// Usage from a test:
//
//	if bad := store.AssertNoStaleReads("../scheduler"); len(bad) > 0 {
//	    t.Errorf("forbidden stale reads: %v", bad)
//	}
//
// Callers pass the package directory; the scan is line-based and only
// matches the literal token "WithStale" (comments included — do not
// mention it casually in sensitive code).
func AssertNoStaleReads(dir string) []string {
	var violations []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			if err == nil && info.IsDir() && info.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "WithStale") {
				violations = append(violations, filepath.Clean(path)+":"+itoa(i+1))
			}
		}
		return nil
	})
	return violations
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
