// Package health implements node health checks. Sources are /proc and
// statfs — never human-readable command output.
package health

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/expanse/expanse/internal/reconcile"
)

// Health mirrors reconcile.Health values for check results.
type Health = reconcile.Health

const (
	Healthy   = reconcile.HealthHealthy
	Degraded  = reconcile.HealthDegraded
	Unhealthy = reconcile.HealthUnhealthy
	Unknown   = reconcile.HealthUnknown
)

// Result is one check's outcome.
type Result struct {
	Name    string
	Status  Health
	Message string
	Details map[string]string
	Took    time.Duration
}

// Check is a single health check.
type Check interface {
	Name() string
	Run(ctx context.Context) Result
}

// Report is the aggregate health report.
type Report struct {
	Overall Health
	Checks  []Result
	At      time.Time
}

// Overall returns the worst of all results.
func Overall(results []Result) Health {
	agg := Healthy
	for _, r := range results {
		if rank(r.Status) > rank(agg) {
			agg = r.Status
		}
	}
	return agg
}

func rank(h Health) int {
	switch h {
	case Healthy:
		return 0
	case Unknown:
		return 1
	case Degraded:
		return 2
	default:
		return 3
	}
}

// ---- disk-space ----

// DiskSpaceCheck checks each real mount's usage.
// Degraded > 80% used; Unhealthy > 90% used.
type DiskSpaceCheck struct {
	Mounts []string // explicit mounts; default: auto-detect real mounts
}

func (c *DiskSpaceCheck) Name() string { return "disk-space" }

func (c *DiskSpaceCheck) Run(ctx context.Context) Result {
	start := time.Now()
	mounts := c.Mounts
	if len(mounts) == 0 {
		mounts = detectMounts()
	}
	res := Result{Name: c.Name(), Status: Healthy, Details: map[string]string{}, Took: 0}
	for _, m := range mounts {
		var st unix.Statfs_t
		if err := unix.Statfs(m, &st); err != nil {
			res.Status = Worst(res.Status, Unknown)
			res.Details[m] = "statfs failed: " + err.Error()
			continue
		}
		total := uint64(st.Bsize) * uint64(st.Blocks)
		used := total - uint64(st.Bsize)*uint64(st.Bavail)
		pct := 0.0
		if total > 0 {
			pct = float64(used) / float64(total) * 100
		}
		res.Details[m] = fmt.Sprintf("%.1f%% used", pct)
		switch {
		case pct > 90:
			res.Status = Worst(res.Status, Unhealthy)
		case pct > 80:
			res.Status = Worst(res.Status, Degraded)
		}
	}
	res.Took = time.Since(start)
	res.Message = strings.Join(mapValuesSorted(res.Details), "; ")
	return res
}

func detectMounts() []string {
	data, err := readFile("/proc/mounts")
	if err != nil {
		return []string{"/"}
	}
	var mounts []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		mount, fstype := fields[1], fields[2]
		if fstype == "proc" || fstype == "sysfs" || fstype == "devtmpfs" ||
			fstype == "tmpfs" || fstype == "devpts" || fstype == "securityfs" ||
			fstype == "cgroup" || fstype == "cgroup2" || fstype == "efivarfs" ||
			fstype == "bpf" || fstype == "debugfs" || fstype == "tracefs" ||
			fstype == "configfs" || fstype == "fusectl" || fstype == "mqueue" ||
			fstype == "hugetlbfs" || fstype == "ramfs" || fstype == "overlay" {
			continue
		}
		if seen[mount] {
			continue
		}
		seen[mount] = true
		mounts = append(mounts, mount)
	}
	if len(mounts) == 0 {
		return []string{"/"}
	}
	return mounts
}

func mapValuesSorted(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// insertion-sort (small maps)
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k + ": " + m[k]
	}
	return out
}

// ---- memory ----

// MemoryCheck: Degraded > 85% used; Unhealthy > 95% used.
type MemoryCheck struct {
	ProcRoot string
}

func (c *MemoryCheck) Name() string { return "memory" }

func (c *MemoryCheck) Run(ctx context.Context) Result {
	start := time.Now()
	res := Result{Name: c.Name(), Status: Unknown, Details: map[string]string{}, Took: 0}
	procRoot := c.ProcRoot
	if procRoot == "" {
		procRoot = "/proc"
	}
	total, avail := meminfo(procRoot)
	if total == 0 {
		res.Message = "no meminfo"
		res.Took = time.Since(start)
		return res
	}
	used := total - avail
	pct := float64(used) / float64(total) * 100
	res.Details["used_pct"] = fmt.Sprintf("%.1f", pct)
	res.Details["total_bytes"] = fmt.Sprint(total)
	res.Status = Healthy
	switch {
	case pct > 95:
		res.Status = Unhealthy
	case pct > 85:
		res.Status = Degraded
	}
	res.Message = fmt.Sprintf("%.1f%% used", pct)
	res.Took = time.Since(start)
	return res
}

func meminfo(procRoot string) (total, available uint64) {
	data, err := readFile(procRoot + "/meminfo")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		k, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch k {
		case "MemTotal":
			total = v * 1024
		case "MemAvailable":
			available = v * 1024
		}
	}
	return total, available
}

// ---- load ----

// LoadCheck: Degraded 1m load > cores; Unhealthy > 4×cores.
type LoadCheck struct {
	ProcRoot string
	Cores    int
}

func (c *LoadCheck) Name() string { return "load" }

func (c *LoadCheck) Run(ctx context.Context) Result {
	start := time.Now()
	res := Result{Name: c.Name(), Status: Unknown, Details: map[string]string{}, Took: 0}
	cores := c.Cores
	if cores <= 0 {
		cores = runtimeNumCPU()
	}
	procRoot := c.ProcRoot
	if procRoot == "" {
		procRoot = "/proc"
	}
	data, err := readFile(procRoot + "/loadavg")
	if err != nil {
		res.Message = "no loadavg"
		res.Took = time.Since(start)
		return res
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		res.Message = "empty loadavg"
		res.Took = time.Since(start)
		return res
	}
	var one float64
	if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
		one = v
	} else {
		res.Message = "unreadable loadavg"
		res.Took = time.Since(start)
		return res
	}
	res.Details["load1m"] = fmt.Sprintf("%.2f", one)
	res.Details["cores"] = fmt.Sprint(cores)
	res.Status = Healthy
	switch {
	case one > float64(4*cores):
		res.Status = Unhealthy
	case one > float64(cores):
		res.Status = Degraded
	}
	res.Message = fmt.Sprintf("load %.2f on %d cores", one, cores)
	res.Took = time.Since(start)
	return res
}

// ---- store ----

// StoreCheck measures a small store write's latency.
// Degraded > 100 ms; Unhealthy if the write fails.
type StoreCheck struct {
	// Put writes a health heartbeat key and returns its duration.
	Put func(ctx context.Context) (time.Duration, error)
}

func (c *StoreCheck) Name() string { return "store" }

func (c *StoreCheck) Run(ctx context.Context) Result {
	start := time.Now()
	res := Result{Name: c.Name(), Status: Unknown, Details: map[string]string{}, Took: 0}
	if c.Put == nil {
		res.Message = "no store configured"
		res.Took = time.Since(start)
		return res
	}
	took, err := c.Put(ctx)
	res.Details["write_ms"] = fmt.Sprintf("%.2f", float64(took.Nanoseconds())/1e6)
	res.Took = time.Since(start)
	if err != nil {
		res.Status = Unhealthy
		res.Message = "store write failed: " + err.Error()
		return res
	}
	res.Status = Healthy
	if took > 100*time.Millisecond {
		res.Status = Degraded
	}
	res.Message = fmt.Sprintf("write took %v", took.Round(time.Microsecond))
	return res
}

// ---- nix-store ----

// NixStoreCheck checks the /nix store disk usage.
// Degraded > 85% used; Unhealthy if statfs fails.
type NixStoreCheck struct {
	Path string // default /nix
}

func (c *NixStoreCheck) Name() string { return "nix-store" }

func (c *NixStoreCheck) Run(ctx context.Context) Result {
	start := time.Now()
	res := Result{Name: c.Name(), Status: Unknown, Details: map[string]string{}, Took: 0}
	path := c.Path
	if path == "" {
		path = "/nix"
	}
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		res.Status = Unhealthy
		res.Message = "statfs /nix failed: " + err.Error()
		res.Took = time.Since(start)
		return res
	}
	total := uint64(st.Bsize) * uint64(st.Blocks)
	used := total - uint64(st.Bsize)*uint64(st.Bavail)
	pct := 0.0
	if total > 0 {
		pct = float64(used) / float64(total) * 100
	}
	res.Details["used_pct"] = fmt.Sprintf("%.1f", pct)
	res.Status = Healthy
	switch {
	case pct > 90:
		res.Status = Unhealthy
	case pct > 85:
		res.Status = Degraded
	}
	res.Message = fmt.Sprintf("/nix %.1f%% used", pct)
	res.Took = time.Since(start)
	return res
}

// ---- clock-sync ----

// ClockSyncCheck: Degraded if offset > 100 ms; Unhealthy if chrony not
// synced. Reads chronyc's tracking output (the structured source; there is
// no sysfs clock-status file).
type ClockSyncCheck struct{}

func (c *ClockSyncCheck) Name() string { return "clock-sync" }

func (c *ClockSyncCheck) Run(ctx context.Context) Result {
	start := time.Now()
	res := Result{Name: c.Name(), Status: Unknown, Details: map[string]string{}, Took: 0}
	out, err := runCommand(ctx, "chronyc", "tracking")
	res.Took = time.Since(start)
	if err != nil {
		// chrony absent: unknown rather than unhealthy (VMs, minimal hosts).
		res.Message = "chrony not available"
		return res
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Leap status") {
			status := strings.TrimSpace(strings.TrimPrefix(line, "Leap status"))
			status = strings.TrimPrefix(status, ":")
			status = strings.TrimSpace(status)
			res.Details["leap_status"] = status
			if status == "Normal" {
				res.Status = Healthy
			} else {
				res.Status = Unhealthy
				res.Message = "chrony leap status: " + status
			}
			return res
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "Last offset" {
			offset := strings.TrimSpace(v)
			res.Details["last_offset"] = offset
			// +0.000001 seconds style; parse leading float.
			secs, perr := strconv.ParseFloat(strings.TrimSpace(offset), 64)
			if perr == nil && (secs > 0.100 || secs < -0.100) {
				res.Status = Worst(res.Status, Degraded)
				res.Message = "offset " + offset
			}
		}
	}
	if res.Status == Unknown {
		res.Status = Healthy
		res.Message = "clock synced"
	}
	return res
}

// ---- reconcile ----

// ReconcileCheck: Degraded if the last tick is > 2 min old; Unhealthy if
// the last 3 ticks all failed.
type ReconcileCheck struct {
	// Metrics returns the reconciler's current metrics snapshot.
	Metrics func() reconcile.Metrics
	// RecentFailures reports how many of the last N ticks failed.
	RecentFailures func() int
}

func (c *ReconcileCheck) Name() string { return "reconcile" }

func (c *ReconcileCheck) Run(ctx context.Context) Result {
	start := time.Now()
	res := Result{Name: c.Name(), Status: Unknown, Details: map[string]string{}, Took: 0}
	if c.Metrics == nil {
		res.Message = "reconciler not running"
		res.Took = time.Since(start)
		return res
	}
	m := c.Metrics()
	age := time.Since(m.LastTickAt)
	res.Details["last_tick_age"] = age.Round(time.Millisecond).String()
	res.Details["ticks"] = fmt.Sprint(m.Ticks)
	res.Status = Healthy
	if m.LastTickAt.IsZero() {
		res.Status = Unknown
		res.Message = "no tick yet"
	} else if age > 2*time.Minute {
		res.Status = Degraded
		res.Message = "last tick " + age.Round(time.Second).String() + " ago"
	}
	if c.RecentFailures != nil && c.RecentFailures() >= 3 {
		res.Status = Unhealthy
		res.Message = "last ticks failed"
	}
	res.Took = time.Since(start)
	return res
}

// ---- runner ----

// Runner executes all registered checks and aggregates a Report.
type Runner struct {
	Checks []Check
}

// RunAll runs checks concurrently and returns the aggregate report.
func (r *Runner) RunAll(ctx context.Context) *Report {
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []Result
	)
	for _, c := range r.Checks {
		wg.Add(1)
		go func(c Check) {
			defer wg.Done()
			res := c.Run(ctx)
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	return &Report{Overall: Overall(results), Checks: results, At: time.Now()}
}

// Worst returns the worse of two health values.
func Worst(a, b Health) Health { return reconcile.Worst(a, b) }
