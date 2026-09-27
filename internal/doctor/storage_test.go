package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/expanse/expanse/internal/storage/drbd"
	"github.com/expanse/expanse/internal/storage/lvm"
)

func TestCheckDRBDModule(t *testing.T) {
	status(t, checkDRBDModule(false, ""), Fail)
	status(t, checkDRBDModule(true, ""), Warn)
	status(t, checkDRBDModule(true, "8.4.11"), Fail)
	status(t, checkDRBDModule(true, "9.2.16"), Pass)
}

func TestCheckVolumeGroup(t *testing.T) {
	status(t, checkVolumeGroup(false, nil, ""), Warn)
	status(t, checkVolumeGroup(true, nil, "not found"), Fail)
	status(t, checkVolumeGroup(true, &lvm.VG{Name: "vg0", Size: 100, Free: 50}, ""), Pass)
	status(t, checkVolumeGroup(true, &lvm.VG{Name: "vg0", Size: 100, Free: 5}, ""), Warn)
}

func TestCheckThinPools(t *testing.T) {
	status(t, checkThinPools(nil), Pass)
	status(t, checkThinPools([]ThinPoolUsage{{Name: "pool", DataPercent: 40, MetaPercent: 10}}), Pass)
	status(t, checkThinPools([]ThinPoolUsage{{Name: "pool", DataPercent: 85, MetaPercent: 10}}), Warn)
	status(t, checkThinPools([]ThinPoolUsage{{Name: "pool", DataPercent: 10, MetaPercent: 92}}), Fail)
}

func TestCheckSystemMirror(t *testing.T) {
	status(t, checkSystemMirror(0, "", nil, "btrfs: not found"), Warn)
	status(t, checkSystemMirror(1, "", nil, ""), Warn)
	status(t, checkSystemMirror(2, "", nil, ""), Pass)
	status(t, checkSystemMirror(2, "RAID5", nil, ""), Fail)
	status(t, checkSystemMirror(1, "RAID6", nil, ""), Fail) // R8 takes priority over the single-device WARN
}

// The mirror layout puts a single-device btrfs on md RAID1: md, not btrfs, holds the second copy.
func TestCheckSystemMirrorOnMD(t *testing.T) {
	status(t, checkSystemMirror(1, "", &MDArray{Name: "md127", Level: "raid1", RaidDisks: 2}, ""), Pass)
	status(t, checkSystemMirror(1, "", &MDArray{Name: "md127", Level: "raid1", RaidDisks: 2, Degraded: 1}, ""), Warn)
	status(t, checkSystemMirror(1, "", &MDArray{Name: "md127", Level: "raid0", RaidDisks: 2}, ""), Warn)
}

func TestBtrfsDevicePaths(t *testing.T) {
	out := "Label: none  uuid: 1234\n\tTotal devices 1 FS bytes used 1.2GiB\n\tdevid    1 size 8.00GiB used 2.0GiB path /dev/md127\n"
	if got := btrfsDevicePaths(out); len(got) != 1 || got[0] != "/dev/md127" {
		t.Fatalf("btrfsDevicePaths = %q, want [/dev/md127]", got)
	}
}

func TestReadMDArray(t *testing.T) {
	sys := t.TempDir()
	md := filepath.Join(sys, "md127", "md")
	if err := os.MkdirAll(md, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, v := range map[string]string{"level": "raid1\n", "raid_disks": "2\n", "degraded": "1\n"} {
		if err := os.WriteFile(filepath.Join(md, f), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := readMDArray(sys, "md127")
	if err != nil {
		t.Fatal(err)
	}
	if want := (MDArray{Name: "md127", Level: "raid1", RaidDisks: 2, Degraded: 1}); *got != want {
		t.Fatalf("readMDArray = %+v, want %+v", *got, want)
	}
	if _, err := readMDArray(sys, "sda"); err == nil {
		t.Fatal("readMDArray on a non-md device: want an error")
	}
}

func TestCheckDRBDResources(t *testing.T) {
	status(t, checkDRBDResources(nil, "drbdsetup: not found"), Warn)
	status(t, checkDRBDResources(nil, ""), Warn)

	healthy := []drbd.Status{{
		Name:    "r0",
		Volumes: []drbd.Volume{{Number: 0, DiskState: drbd.DiskUpToDate, Quorum: true}},
		Peers:   []drbd.Peer{{Name: "n2", Connection: drbd.ConnConnected}},
	}}
	status(t, checkDRBDResources(healthy, ""), Pass)

	noQuorum := []drbd.Status{{
		Name:    "r0",
		Volumes: []drbd.Volume{{Number: 0, DiskState: drbd.DiskUpToDate, Quorum: false}},
	}}
	status(t, checkDRBDResources(noQuorum, ""), Fail)

	degradedDisk := []drbd.Status{{
		Name:    "r0",
		Volumes: []drbd.Volume{{Number: 0, DiskState: drbd.DiskInconsistent, Quorum: true}},
	}}
	status(t, checkDRBDResources(degradedDisk, ""), Fail)

	standalonePeer := []drbd.Status{{
		Name:    "r0",
		Volumes: []drbd.Volume{{Number: 0, DiskState: drbd.DiskUpToDate, Quorum: true}},
		Peers:   []drbd.Peer{{Name: "n2", Connection: drbd.ConnStandAlone}},
	}}
	status(t, checkDRBDResources(standalonePeer, ""), Fail)
}

func TestRunStorageReturnsAllFiveRows(t *testing.T) {
	rs := RunStorage(StorageInput{ModuleLoaded: true, ModuleVersion: "9.2.16"})
	if len(rs) != 5 {
		t.Fatalf("got %d rows, want 5: %+v", len(rs), rs)
	}
}
