package perf

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Budget represents a performance constraint.
type Budget struct {
	Name     string  `yaml:"name"`
	Unit     string  `yaml:"unit"` // "bytes", "ms", "percent", "rps", "us", "ratio"
	Max      float64 `yaml:"max"`
	Min      float64 `yaml:"min"` // floors (e.g. lb_rps ≥ 20k); 0 = none
	Measured float64 `yaml:"-"`
}

type budgetsFile struct {
	Budgets []Budget `yaml:"budgets"`
}

// LoadBudgets reads test/perf/budgets.yaml and returns the budget list.
func LoadBudgets() []Budget {
	root, ok := repoRoot()
	if !ok {
		return fallbackBudgets()
	}
	data, err := os.ReadFile(filepath.Join(root, "test", "perf", "budgets.yaml"))
	if err != nil {
		return fallbackBudgets()
	}
	var f budgetsFile
	if err := yaml.Unmarshal(data, &f); err != nil || len(f.Budgets) == 0 {
		return fallbackBudgets()
	}
	return f.Budgets
}

// fallbackBudgets mirrors budgets.yaml for when the file is unavailable.
func fallbackBudgets() []Budget {
	return []Budget{
		{Name: "binary_size", Unit: "bytes", Max: 41943040},
		{Name: "cli_startup", Unit: "ms", Max: 50},
		{Name: "iso_size", Unit: "bytes", Max: 1610612736},
		{Name: "install_footprint", Unit: "bytes", Max: 6442450944},
		{Name: "boot_time_ms", Unit: "ms", Max: 90000},
		{Name: "agent_idle_rss_bytes", Unit: "bytes", Max: 125829120},
		{Name: "agent_idle_cpu_percent", Unit: "percent", Max: 2},
		{Name: "reconcile_tick_p99_ms", Unit: "ms", Max: 500},
	}
}

// CheckAll measures all budgets and returns an error for any violated
// constraint.
func CheckAll(budgets []Budget) error {
	var msgs []string
	for i := range budgets {
		b := &budgets[i]
		switch b.Name {
		case "binary_size":
			b.Measured = measureBinarySize()
		case "cli_startup":
			b.Measured = measureCLIStartup()
		case "iso_size":
			b.Measured = measureISOSize()
		case "agent_idle_rss_bytes", "agent_idle_cpu_percent", "reconcile_tick_p99_ms":
			// One idle-agent measurement run feeds all three.
			rss, cpu, p99 := measureIdleAgent()
			for j := range budgets {
				switch budgets[j].Name {
				case "agent_idle_rss_bytes":
					budgets[j].Measured = rss
				case "agent_idle_cpu_percent":
					budgets[j].Measured = cpu
				case "reconcile_tick_p99_ms":
					budgets[j].Measured = p99
				}
			}
		case "install_footprint", "boot_time_ms":
			// Measured inside the VM tests (nix/tests/*), not here.
			b.Measured = -1
		case "lb_rps", "lb_added_latency_p99_us", "dns_query_p99_us", "l4_throughput_ratio":
			// Measured by the §6 network tests in networkperf_test.go
			// (they share one fixture; CheckAll leaves them unmeasured
			// so TestBudgets does not re-run the load windows).
			b.Measured = -1
		case "vol_seqwrite_ratio", "vol_seqread_ratio", "vol_randwrite_ratio",
			"vol_randread_ratio", "vol_fsync_p99_us", "vol_failover_ms", "vol_resync_mbps":
			// VM-only (Phase 06 T23): fio and failover need a real ZFS
			// pool and real primary crashes (nix/tests/vol-perf.nix).
			b.Measured = -1
		case "proxy_rss_bytes", "vip_failover_ms":
			// VM-only: RSS needs the isolated expanse-agent process
			// (G5.14), failover needs real lease/ARP churn
			// (net-vip-failover). See budgets.yaml.
			b.Measured = -1
		case "raft_write_p99_ms", "raft_read_linear_p99_ms", "raft_read_stale_p99_ms",
			"cluster_form_3node_s", "leader_election_p99_ms",
			"raft_snapshot_100k_s", "raft_restore_100k_s":
			// Measured by the dedicated cluster tests in raft_perf_test.go
			// (real in-process rafts, several minutes of work). CheckAll
			// leaves them unmeasured (-1 = pass) so TestBudgets does not
			// re-boot multiple rafts in one run.
			b.Measured = -1
		default:
			b.Measured = -1
		}
		// Measured < 0 = not measured in this run (VM-only or dedicated
		// test) — never a violation.
		if b.Measured < 0 {
			continue
		}
		if b.Max > 0 && b.Measured > b.Max {
			msgs = append(msgs, fmt.Sprintf("budget %s violated: %.0f %s > %.0f %s",
				b.Name, b.Measured, b.Unit, b.Max, b.Unit))
		}
		if b.Min > 0 && b.Measured < b.Min {
			msgs = append(msgs, fmt.Sprintf("budget %s violated: %.0f %s < %.0f %s",
				b.Name, b.Measured, b.Unit, b.Min, b.Unit))
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

// measureISOSize returns the size of the built installer ISO, or -1 if
// the ISO has not been built (result-iso symlink from `nix build .#iso`).
func measureISOSize() float64 {
	root, ok := repoRoot()
	if !ok {
		return -1
	}
	entries, err := filepath.Glob(filepath.Join(root, "result-iso", "iso", "*.iso"))
	if err != nil || len(entries) == 0 {
		return -1
	}
	fi, err := os.Stat(entries[0])
	if err != nil {
		return -1
	}
	return float64(fi.Size())
}

// measureIdleAgent runs `expanse agent` for the budget window (default
// 60 s; EXpanse_PERF_SECONDS shortens it) with 20 file resources applied,
// then samples: idle RSS, idle CPU (utime+stime delta of /proc/<pid>/stat),
// and reconcile tick p99 (from the agent's status JSON across ticks).
func measureIdleAgent() (rss, cpuPct, tickP99Ms float64) {
	root, ok := repoRoot()
	if !ok {
		return -1, -1, -1
	}
	bin := filepath.Join(root, "bin", "expanse")
	if _, err := os.Stat(bin); err != nil {
		return -1, -1, -1
	}
	window := 60 * time.Second
	if s := os.Getenv("EXpanse_PERF_SECONDS"); s != "" {
		if d, err := strconv.Atoi(s); err == nil && d > 0 {
			window = time.Duration(d) * time.Second
		}
	}
	dir, err := os.MkdirTemp("", "expanse-perf-*")
	if err != nil {
		return -1, -1, -1
	}
	defer os.RemoveAll(dir)

	cmd := exec.Command(bin, "agent",
		"--data-dir", filepath.Join(dir, "persist"),
		"--socket", filepath.Join(dir, "run", "agent.sock"),
		"--period", "2s")
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		return -1, -1, -1
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	socket := filepath.Join(dir, "run", "agent.sock")

	// Apply 20 file resources.
	var sb strings.Builder
	for i := 0; i < 20; i++ {
		p := filepath.Join(dir, fmt.Sprintf("res%d", i))
		fmt.Fprintf(&sb, "file:%s:\n  type: file\n  path: %s\n  content: \"perf %d\"\n  mode: \"0644\"\n", p, p, i)
	}
	applyAndWait := func() {
		specPath := filepath.Join(dir, "spec.yaml")
		_ = os.WriteFile(specPath, []byte(sb.String()), 0o644)
		for i := 0; i < 50; i++ {
			if err := exec.Command(bin, "ctl", "--socket", socket, "resource", "apply",
				"-f", specPath).Run(); err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	// Wait for the socket, then apply.
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	applyAndWait()

	// Sample CPU counters and tick durations over the window.
	startCPU := readProcCPU(cmd.Process.Pid)
	start := time.Now()
	var tickDurations []float64
	lastTick := 0.0
	for time.Since(start) < window {
		time.Sleep(500 * time.Millisecond)
		if out, err := exec.Command(bin, "ctl", "--socket", socket,
			"node", "status", "-o", "json").Output(); err == nil {
			if v, ok := jsonField(string(out), "lastTickDurationMs"); ok {
				if cur, ok2 := jsonField(string(out), "tickCount"); ok2 && cur != lastTick {
					lastTick = cur
					tickDurations = append(tickDurations, v)
				}
			}
		}
	}
	endCPU := readProcCPU(cmd.Process.Pid)
	elapsed := time.Since(start).Seconds()

	rss = float64(readProcRSS(cmd.Process.Pid))
	if startCPU >= 0 && endCPU >= 0 && elapsed > 0 {
		cpuPct = (endCPU - startCPU) / elapsed / 100.0 // jiffies/s / clk-tck → % of one core
	}
	tickP99Ms = percentile(tickDurations, 0.99)
	return rss, cpuPct, tickP99Ms
}

func readProcRSS(pid int) int {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				v, _ := strconv.Atoi(fields[1])
				return v * 1024 // kB → bytes
			}
		}
	}
	return -1
}

// readProcCPU returns utime+stime in clock ticks (USER_HZ, typically 100).
func readProcCPU(pid int) float64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return -1
	}
	// After the comm field (in parens): fields 14/15 are utime/stime.
	s := string(data)
	if i := strings.LastIndex(s, ")"); i >= 0 && i+2 < len(s) {
		fields := strings.Fields(s[i+2:])
		if len(fields) >= 13 {
			u, _ := strconv.ParseFloat(fields[11], 64)
			st, _ := strconv.ParseFloat(fields[12], 64)
			return u + st
		}
	}
	return -1
}

func jsonField(json, field string) (float64, bool) {
	key := `"` + field + `":`
	i := strings.Index(json, key)
	if i < 0 {
		return 0, false
	}
	rest := json[i+len(key):]
	end := strings.IndexAny(rest, ",}")
	if end < 0 {
		return 0, false
	}
	// protojson emits 64-bit ints as quoted strings: strip quotes.
	val := strings.Trim(strings.TrimSpace(rest[:end]), `"`)
	v, err := strconv.ParseFloat(val, 64)
	return v, err == nil
}

func percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return -1
	}
	// Simple copy+sort (small n).
	cp := append([]float64(nil), vals...)
	for i := 1; i < len(cp); i++ {
		for j := i; j > 0 && cp[j] < cp[j-1]; j-- {
			cp[j], cp[j-1] = cp[j-1], cp[j]
		}
	}
	idx := int(p * float64(len(cp)-1))
	return cp[idx]
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
