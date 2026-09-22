package doctor

import (
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
	status(t, checkSystemMirror(0, "", "btrfs: not found"), Warn)
	status(t, checkSystemMirror(1, "", ""), Warn)
	status(t, checkSystemMirror(2, "", ""), Pass)
	status(t, checkSystemMirror(2, "RAID5", ""), Fail)
	status(t, checkSystemMirror(1, "RAID6", ""), Fail) // R8 takes priority over the single-device WARN
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
