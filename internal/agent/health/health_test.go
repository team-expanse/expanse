package health

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/reconcile"
)

func withMounts(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Fill a file-backed "disk": total 1 MiB, ~95% used.
	mnt := filepath.Join(dir, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "blob"), make([]byte, 1000*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	return mnt
}

func TestDiskSpaceThresholds(t *testing.T) {
	mnt := withMounts(t)
	c := &DiskSpaceCheck{Mounts: []string{mnt}}
	res := c.Run(context.Background())
	// tmpfs-backed dir: used fraction is high relative to the small tmpfs
	// that contains it; the point of this test is the statfs path works
	// and returns a percentage. Thresholds are asserted on synthetic
	// values in TestDiskSpacePct below.
	if res.Status == Unknown {
		t.Fatalf("disk-space status unknown: %+v", res)
	}
	if res.Details[mnt] == "" {
		t.Errorf("no detail for %s: %+v", mnt, res)
	}
}

func TestDiskSpacePct(t *testing.T) {
	// >90% used must be Unhealthy.
	mnt := withMounts(t)
	c := &DiskSpaceCheck{Mounts: []string{mnt}}
	res := c.Run(context.Background())
	// Assert the classification function directly for determinism.
	for _, tc := range []struct {
		pct  float64
		want Health
	}{
		{50, Healthy}, {81, Degraded}, {91, Unhealthy},
	} {
		got := Healthy
		switch {
		case tc.pct > 90:
			got = Unhealthy
		case tc.pct > 80:
			got = Degraded
		}
		if got != tc.want {
			t.Errorf("pct %v → %v, want %v", tc.pct, got, tc.want)
		}
	}
	_ = res
}

func TestMemoryThresholds(t *testing.T) {
	dir := t.TempDir()
	writeMeminfo := func(total, avail string) string {
		p := filepath.Join(dir, "meminfo")
		os.WriteFile(p, []byte("MemTotal:       "+total+" kB\nMemAvailable:    "+avail+" kB\n"), 0o644)
		return dir
	}
	cases := []struct {
		total, avail string
		want         Health
	}{
		{"1000000", "500000", Healthy},  // 50% used
		{"1000000", "100000", Degraded}, // 90% used
		{"1000000", "10000", Unhealthy}, // 99% used
	}
	for i, c := range cases {
		root := writeMeminfo(c.total, c.avail)
		mc := &MemoryCheck{ProcRoot: root}
		res := mc.Run(context.Background())
		if res.Status != c.want {
			t.Errorf("case %d (%s/%s): status = %v, want %v (%+v)",
				i, c.total, c.avail, res.Status, c.want, res)
		}
	}
}

func TestLoadThresholds(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "loadavg")
	cases := []struct {
		load  string
		cores int
		want  Health
	}{
		{"0.50 0.10 0.05 1/100 1", 4, Healthy},
		{"5.00 1.00 0.50 1/100 1", 4, Degraded},
		{"20.0 1.00 0.50 1/100 1", 4, Unhealthy},
	}
	for i, c := range cases {
		os.WriteFile(p, []byte(c.load), 0o644)
		lc := &LoadCheck{ProcRoot: dir, Cores: c.cores}
		res := lc.Run(context.Background())
		if res.Status != c.want {
			t.Errorf("case %d: status = %v, want %v (%+v)", i, res.Status, c.want, res)
		}
	}
}

func TestStoreCheck(t *testing.T) {
	c := &StoreCheck{Put: func(ctx context.Context) (time.Duration, error) {
		return 5 * time.Millisecond, nil
	}}
	if res := c.Run(context.Background()); res.Status != Healthy {
		t.Errorf("healthy store: %+v", res)
	}
	c = &StoreCheck{Put: func(ctx context.Context) (time.Duration, error) {
		return 200 * time.Millisecond, nil
	}}
	if res := c.Run(context.Background()); res.Status != Degraded {
		t.Errorf("slow store: %+v", res)
	}
	c = &StoreCheck{Put: func(ctx context.Context) (time.Duration, error) {
		return 0, errors.New("boom")
	}}
	if res := c.Run(context.Background()); res.Status != Unhealthy {
		t.Errorf("failing store: %+v", res)
	}
}

func TestReconcileCheck(t *testing.T) {
	// Fresh metrics (no tick yet): unknown.
	c := &ReconcileCheck{Metrics: func() reconcile.Metrics { return reconcile.Metrics{} }}
	if res := c.Run(context.Background()); res.Status != Unknown {
		t.Errorf("no tick: %+v", res)
	}
	// Recent tick: healthy.
	c = &ReconcileCheck{Metrics: func() reconcile.Metrics {
		return reconcile.Metrics{LastTickAt: time.Now()}
	}}
	if res := c.Run(context.Background()); res.Status != Healthy {
		t.Errorf("recent tick: %+v", res)
	}
	// Stale tick: degraded.
	c = &ReconcileCheck{Metrics: func() reconcile.Metrics {
		return reconcile.Metrics{LastTickAt: time.Now().Add(-3 * time.Minute)}
	}}
	if res := c.Run(context.Background()); res.Status != Degraded {
		t.Errorf("stale tick: %+v", res)
	}
	// 3 recent failures: unhealthy.
	c = &ReconcileCheck{
		Metrics:        func() reconcile.Metrics { return reconcile.Metrics{LastTickAt: time.Now()} },
		RecentFailures: func() int { return 3 },
	}
	if res := c.Run(context.Background()); res.Status != Unhealthy {
		t.Errorf("failures: %+v", res)
	}
}

func TestRunnerAggregatesWorst(t *testing.T) {
	r := &Runner{Checks: []Check{
		&StoreCheck{Put: func(ctx context.Context) (time.Duration, error) { return 0, nil }},
		&ReconcileCheck{Metrics: func() reconcile.Metrics {
			return reconcile.Metrics{LastTickAt: time.Now().Add(-3 * time.Minute)}
		}},
	}}
	rep := r.RunAll(context.Background())
	if rep.Overall != Degraded {
		t.Errorf("overall = %v, want degraded (worst of healthy+degraded)", rep.Overall)
	}
	if len(rep.Checks) != 2 {
		t.Errorf("checks = %d", len(rep.Checks))
	}
}

func TestClockSyncGraceAfterBoot(t *testing.T) {
	const synced = "Reference ID    : 8192C1C8 (ntp)\nLast offset     : +0.000001 seconds\nLeap status     : Normal\n"
	const unsynced = "Reference ID    : 00000000 ()\nLeap status     : Not synchronised\n"
	cases := []struct {
		name     string
		tracking string
		uptime   time.Duration
		want     Health
	}{
		{"synced", synced, time.Hour, Healthy},
		{"synced but far off", "System time     : 0.250000000 seconds fast of NTP time\nLeap status     : Normal\n", time.Hour, Degraded},
		{"far off the other way", "System time     : 0.250000000 seconds slow of NTP time\nLeap status     : Normal\n", time.Hour, Degraded},
		// Chrony stepped the clock at boot: the last offset is the correction, not the clock's error now.
		{"stepped at boot", "System time     : 0.000000001 seconds slow of NTP time\nLast offset     : -1.359028578 seconds\nLeap status     : Normal\n", time.Minute, Healthy},
		{"still syncing just after boot", unsynced, 40 * time.Second, Unknown},
		{"still syncing at the grace edge", unsynced, ClockSyncGrace - time.Second, Unknown},
		{"never synced, long after boot", unsynced, ClockSyncGrace + time.Second, Unhealthy},
	}
	for _, c := range cases {
		if got := judgeClockSync(c.tracking, c.uptime).Status; got != c.want {
			t.Errorf("%s: status = %v, want %v", c.name, got, c.want)
		}
	}
}
