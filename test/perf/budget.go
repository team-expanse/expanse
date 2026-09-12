package perf

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Budget represents a performance constraint.
type Budget struct {
	Name     string
	Unit     string // "bytes", "ms", "percent"
	Max      float64
	Measured float64
}

// LoadBudgets reads test/perf/budgets.yaml and returns the list of budgets.
func LoadBudgets() []Budget {
	return []Budget{
		{Name: "binary_size", Unit: "bytes", Max: 41943040},   // 40 MiB
		{Name: "cli_startup", Unit: "ms", Max: 50},          // 50 ms
	}
}

// CheckAll measures all budgets and returns errors for any violated constraints.
func CheckAll(budgets []Budget) error {
	var msgs []string
	for i := range budgets {
		b := &budgets[i]
		switch b.Name {
		case "binary_size":
			b.Measured = measureBinarySize()
		case "cli_startup":
			b.Measured = measureCLIStartup()
		}
		if b.Measured > b.Max {
			msgs = append(msgs, fmt.Sprintf("budget %s violated: %.0f %s > %.0f %s", b.Name, b.Measured, b.Unit, b.Max, b.Unit))
		}
	}
	if len(msgs) > 0 {
		return fmt.Errorf("%s", msgs[0])
	}
	return nil
}

func measureBinarySize() float64 {
	root, ok := repoRoot()
	if !ok {
		return -1
	}
	fi, err := os.Stat(filepath.Join(root, "bin", "expanse"))
	if err != nil {
		return -1
	}
	return float64(fi.Size())
}

func measureCLIStartup() float64 {
	root, ok := repoRoot()
	if !ok {
		return -1
	}
	binPath := filepath.Join(root, "bin", "expanse")
	var total time.Duration
	const runs = 20
	for i := 0; i < runs; i++ {
		start := time.Now()
		if err := exec.Command(binPath, "version").Run(); err != nil {
			return -1
		}
		total += time.Since(start)
	}
	return float64(total.Milliseconds()) / runs
}

// repoRoot walks up from the current directory to find the module root
// (the directory containing go.mod).
func repoRoot() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}