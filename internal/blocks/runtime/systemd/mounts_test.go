package systemd

import (
	"context"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/reconcile"
)

// mountedOnly reports just the listed paths as mount points.
func mountedOnly(paths ...string) func(string) (bool, error) {
	return func(p string) (bool, error) {
		for _, m := range paths {
			if m == p {
				return true, nil
			}
		}
		return false, nil
	}
}

func TestAReplicaWaitsForItsVolumeMounts(t *testing.T) {
	api := &fakeUnitAPI{states: map[string][3]string{}, status: map[string]string{}}
	spec := sampleSpec()
	m := NewManager(api, &Applier{Cache: newTestCache(t), Builder: &mockBuilder{}})
	m.SpecDir = t.TempDir()
	m.Mounted = mountedOnly(spec.VolumeMounts[0])
	res := load(t, m, spec)

	o, err := m.Observe(context.Background(), res)
	if err != nil {
		t.Fatal(err)
	}
	if o.InSync || o.Health != reconcile.HealthDegraded || o.Ready == nil || *o.Ready {
		t.Errorf("unmounted: in_sync=%v health=%v ready=%v, want out of sync, degraded, not ready", o.InSync, o.Health, o.Ready)
	}
	if want := "waiting for volume mount " + spec.VolumeMounts[1]; o.Details["status"] != want {
		t.Errorf("status = %q, want %q", o.Details["status"], want)
	}
	converge(t, m, res)
	if api.starts != 0 {
		t.Fatal("the replica started before its volume was mounted")
	}

	m.Mounted = mountedOnly(spec.VolumeMounts...)
	converge(t, m, res)
	if api.starts != 1 {
		t.Fatalf("starts = %d after the mounts appeared, want 1", api.starts)
	}
	if o, _ := m.Observe(context.Background(), res); !o.InSync {
		t.Error("a mounted, running replica is out of sync")
	}
}

func TestARunningReplicaThatLostItsMountIsNotReady(t *testing.T) {
	api := &fakeUnitAPI{states: map[string][3]string{}, status: map[string]string{}}
	spec := sampleSpec()
	m, res := converged(t, api, spec)
	m.Mounted = mountedOnly()

	o, err := m.Observe(context.Background(), res)
	if err != nil {
		t.Fatal(err)
	}
	if o.InSync || o.Ready == nil || *o.Ready {
		t.Errorf("mount gone: in_sync=%v ready=%v, want out of sync and not ready", o.InSync, o.Ready)
	}
	acts, err := m.Plan(context.Background(), res, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 0 {
		t.Errorf("planned %d actions without the mount, want none", len(acts))
	}
}

func TestASuspendedReplicaIgnoresItsMounts(t *testing.T) {
	api := &fakeUnitAPI{states: map[string][3]string{}, status: map[string]string{}}
	spec := sampleSpec()
	spec.Stopped = true
	m, res := converged(t, api, spec)
	m.Mounted = mountedOnly()
	o, err := m.Observe(context.Background(), res)
	if err != nil {
		t.Fatal(err)
	}
	if !o.InSync {
		t.Error("a stopped replica waits on its mounts")
	}
}

func TestMountPointReadsMountinfo(t *testing.T) {
	info := strings.Join([]string{
		"22 1 253:1 / / rw,relatime shared:1 - ext4 /dev/vda1 rw",
		"95 22 147:0 / /var/lib/expanse/volumes/vol-a/mnt rw,relatime shared:50 - ext4 /dev/drbd0 rw",
		`96 22 147:1 / /mnt/with\040space rw - xfs /dev/drbd1 rw`,
	}, "\n")
	for path, want := range map[string]bool{
		"/var/lib/expanse/volumes/vol-a/mnt":  true,
		"/var/lib/expanse/volumes/vol-a/mnt/": true,
		"/mnt/with space":                     true,
		"/var/lib/expanse/volumes/vol-b/mnt":  false,
		"/var/lib/expanse/volumes":            false,
	} {
		got, err := mountPointIn(strings.NewReader(info), path)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("mountPointIn(%q) = %v, want %v", path, got, want)
		}
	}
}
