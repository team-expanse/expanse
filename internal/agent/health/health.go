// Package health implements node health checks. Sources are /proc and
// statfs — never human-readable command output.
package health

import (
	"context"
	"fmt"
	"sort"
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

// blocksPlacement names the checks whose failure stops a node running workloads. The rest
// (clock-sync, load, reconcile) are reported and alerted on, but a node failing them still takes work.
var blocksPlacement = map[string]bool{"disk-space": true, "memory": true, "nix-store": true, "store": true}

// Schedulable reports whether a node should take placements: no placement-blocking check is unhealthy.
func Schedulable(results []Result) bool {
	for _, r := range results {
		if blocksPlacement[r.Name] && r.Status == Unhealthy {
			return false
		}
	}
	return true
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

// DiskSpaceCheck checks each real filesystem's usage.
// Degraded > 80% used; Unhealthy > 90% used.
type DiskSpaceCheck struct {
	Mounts []string // explicit mounts; default: one mount per detected filesystem
}

func (c *DiskSpaceCheck) Name() string { return "disk-space" }

func (c *DiskSpaceCheck) Run(ctx context.Context) Result {
	start := time.Now()
	mounts := c.Mounts
	if len(mounts) == 0 {
		mounts = detectMounts()
	}
	res := Result{Name: c.Name(), Status: Healthy, Details: map[string]string{}, Took: 0}
	used := map[string]float64{}
	var failed []string
	for _, m := range mounts {
		var st unix.Statfs_t
		if err := unix.Statfs(m, &st); err != nil {
			res.Status = Worst(res.Status, Unknown)
			res.Details[m] = "statfs failed: " + err.Error()
			failed = append(failed, m+": "+res.Details[m])
			continue
		}
		total := uint64(st.Bsize) * uint64(st.Blocks)
		pct := 0.0
		if total > 0 {
			pct = float64(total-uint64(st.Bsize)*uint64(st.Bavail)) / float64(total) * 100
		}
		used[m] = pct
		res.Details[m] = fmt.Sprintf("%.1f%% used", pct)
		switch {
		case pct > 90:
			res.Status = Worst(res.Status, Unhealthy)
		case pct > 80:
			res.Status = Worst(res.Status, Degraded)
		}
	}
	res.Took = time.Since(start)
	res.Message = strings.Join(append(failed, diskMessage(used)), "; ")
	return res
}

// diskMessage lists each mount's usage, the fullest first.
func diskMessage(used map[string]float64) string {
	mounts := make([]string, 0, len(used))
	for m := range used {
		mounts = append(mounts, m)
	}
	sort.Slice(mounts, func(i, j int) bool {
		if used[mounts[i]] != used[mounts[j]] {
			return used[mounts[i]] > used[mounts[j]]
		}
		return mounts[i] < mounts[j]
	})
	parts := make([]string, len(mounts))
	for i, m := range mounts {
		parts[i] = fmt.Sprintf("%s %.1f%% used", m, used[m])
	}
	return strings.Join(parts, ", ")
}

func detectMounts() []string {
	data, err := readFile("/proc/mounts")
	if err != nil {
		return []string{"/"}
	}
	if mounts := filesystems(string(data)); len(mounts) > 0 {
		return mounts
	}
	return []string{"/"}
}

// notDisk are filesystem types with no disk space to run out of, or none that can grow (images).
var notDisk = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "tmpfs": true, "devpts": true, "securityfs": true,
	"cgroup": true, "cgroup2": true, "efivarfs": true, "bpf": true, "debugfs": true, "tracefs": true,
	"configfs": true, "fusectl": true, "mqueue": true, "hugetlbfs": true, "ramfs": true, "overlay": true,
	"pstore": true, "autofs": true, "binfmt_misc": true, "nsfs": true, "rpc_pipefs": true,
	"squashfs": true, "iso9660": true,
}

// filesystems picks one mount per filesystem from /proc/mounts, its shortest mount point, sorted:
// bind mounts and btrfs subvolumes share their device's space.
func filesystems(procMounts string) []string {
	best := map[string]string{}
	for _, line := range strings.Split(procMounts, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || notDisk[fields[2]] {
			continue
		}
		dev, mount := fields[0]+" "+fields[2], fields[1]
		if cur, ok := best[dev]; !ok || len(mount) < len(cur) || (len(mount) == len(cur) && mount < cur) {
			best[dev] = mount
		}
	}
	out := make([]string, 0, len(best))
	for _, m := range best {
		out = append(out, m)
	}
	sort.Strings(out)
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

// ClockSyncGrace is how long after boot chrony may still be synchronising before an
// unsynchronised clock counts as unhealthy (and fires ExpanseNodeUnhealthy).
const ClockSyncGrace = 10 * time.Minute

// ClockSyncCheck: Degraded if the clock is > 100 ms off; Unhealthy if chrony is not synced,
// except within ClockSyncGrace of boot, when it is still Unknown. Reads chronyc tracking.
type ClockSyncCheck struct{}

func (c *ClockSyncCheck) Name() string { return "clock-sync" }

func (c *ClockSyncCheck) Run(ctx context.Context) Result {
	start := time.Now()
	out, err := runCommand(ctx, "chronyc", "tracking")
	if err != nil {
		// chrony absent: unknown rather than unhealthy (VMs, minimal hosts).
		return Result{Name: c.Name(), Status: Unknown, Message: "chrony not available", Details: map[string]string{}, Took: time.Since(start)}
	}
	res := judgeClockSync(string(out), systemUptime())
	res.Took = time.Since(start)
	return res
}

// judgeClockSync turns chronyc tracking output into a result, given the time since boot.
func judgeClockSync(tracking string, uptime time.Duration) Result {
	res := Result{Name: "clock-sync", Status: Unknown, Details: map[string]string{}}
	for _, line := range strings.Split(tracking, "\n") {
		k, v, _ := strings.Cut(line, ":")
		switch strings.TrimSpace(k) {
		case "Last offset": // the last correction (a boot-time step included), not the clock's error
			res.Details["last_offset"] = strings.TrimSpace(v)
		case "System time": // "0.000000001 seconds slow of NTP time": the error now
			offset := strings.TrimSpace(v)
			res.Details["system_time"] = offset
			secs, perr := strconv.ParseFloat(strings.Fields(offset + " x")[0], 64)
			if perr == nil && (secs > 0.100 || secs < -0.100) {
				res.Status = Worst(res.Status, Degraded)
				res.Message = "clock " + offset
			}
		}
		if status, ok := strings.CutPrefix(line, "Leap status"); ok {
			status = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(status), ":"))
			res.Details["leap_status"] = status
			switch {
			case status == "Normal" && res.Status == Unknown:
				res.Status, res.Message = Healthy, "clock synced"
			case status == "Normal":
			case uptime < ClockSyncGrace:
				res.Status, res.Message = Unknown, "synchronising since boot"
			default:
				res.Status, res.Message = Unhealthy, "chrony leap status: "+status
			}
			return res
		}
	}
	if res.Status == Unknown {
		res.Status, res.Message = Healthy, "clock synced"
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

// RunAll runs checks concurrently and returns the aggregate report, its
// checks in registration order so every run lists them the same way.
func (r *Runner) RunAll(ctx context.Context) *Report {
	var wg sync.WaitGroup
	results := make([]Result, len(r.Checks))
	for i, c := range r.Checks {
		wg.Add(1)
		go func(i int, c Check) {
			defer wg.Done()
			results[i] = c.Run(ctx)
		}(i, c)
	}
	wg.Wait()
	return &Report{Overall: Overall(results), Checks: results, At: time.Now()}
}

// Worst returns the worse of two health values.
func Worst(a, b Health) Health { return reconcile.Worst(a, b) }
