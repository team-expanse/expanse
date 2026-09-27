// `expanse doctor storage` (A5): DRBD module and version, the LVM volume
// group's headroom, thin pool Data%/Meta% exhaustion, the system btrfs
// volume's mirror state (detect-only per ARCHITECTURE.md §3.4 — a
// single-device system volume cannot self-heal, only warn; RAID5/6 is a
// harder FAIL, per R8), and every configured DRBD resource's disk/quorum state.
//
// Same split as network.go: RunStorage's checkXxx functions are pure
// decisions over StorageInput, covered by unit tests; CollectStorage is
// the thin OS adapter, exercised by the VM test.
package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/expanse/expanse/internal/storage/drbd"
	"github.com/expanse/expanse/internal/storage/lvm"
)

const (
	thinFailPercent = 90 // Data%/Meta% at or above this: pool exhaustion is imminent
	thinWarnPercent = 80
	vgWarnFreePct   = 10 // VG free space at or below this: no headroom for new pools/thick LVs
)

// ThinPoolUsage is one thin pool's fill level.
type ThinPoolUsage struct {
	Name                     string
	DataPercent, MetaPercent float64
}

// StorageInput is everything the storage checks decide over. CollectStorage fills
// it; tests construct it by hand.
type StorageInput struct {
	// 1: DRBD kernel module.
	ModuleLoaded  bool
	ModuleVersion string // e.g. "9.2.16"; "" if unknown

	// 2: the LVM volume group backing replicas.
	VGRequested bool // false when no --vg was given: the row degrades to WARN, not FAIL
	VG          *lvm.VG
	VGErr       string

	// 3: thin pools inside the VG.
	Pools []ThinPoolUsage

	// 4: the system btrfs volume's device count (1 = unmirrored) and RAID profile.
	SystemDevices     int
	SystemRAIDProfile string   // "RAID5"/"RAID6" if in use (R8); "" otherwise
	SystemMD          *MDArray // the md array under a single-device btrfs, if any (the mirror layout)
	SystemErr         string

	// 5: every configured DRBD resource.
	Resources    []drbd.Status
	ResourcesErr string
}

// RunStorage executes all 5 storage checks.
func RunStorage(in StorageInput) []Result {
	return []Result{
		checkDRBDModule(in.ModuleLoaded, in.ModuleVersion),
		checkVolumeGroup(in.VGRequested, in.VG, in.VGErr),
		checkThinPools(in.Pools),
		checkSystemMirror(in.SystemDevices, in.SystemRAIDProfile, in.SystemMD, in.SystemErr),
		checkDRBDResources(in.Resources, in.ResourcesErr),
	}
}

func checkDRBDModule(loaded bool, version string) Result {
	if !loaded {
		return Result{
			"drbd-module", Fail, "not loaded",
			"modprobe drbd; check boot.kernelModules and boot.extraModulePackages carry the drbd9 kernel package",
		}
	}
	if version == "" {
		return Result{"drbd-module", Warn, "loaded, version unknown", "check /sys/module/drbd/version"}
	}
	if !strings.HasPrefix(version, "9.") {
		return Result{
			"drbd-module", Fail, "version " + version + ", want 9.x",
			"pin the drbd9 kernel package (R6) — DRBD 8 resource files are not compatible",
		}
	}
	return Result{"drbd-module", Pass, "loaded, version " + version, ""}
}

func checkVolumeGroup(requested bool, vg *lvm.VG, errStr string) Result {
	if !requested {
		return Result{"volume-group", Warn, "no --vg given", "pass --vg <name> to check LVM capacity"}
	}
	if errStr != "" || vg == nil {
		return Result{
			"volume-group", Fail, "not found: " + errStr,
			"check disko/LVM config; the volume group named by --storage-vg must exist",
		}
	}
	freePct := 100 * float64(vg.Free) / float64(vg.Size)
	detail := fmt.Sprintf("%s: %.1f%% free of %d bytes", vg.Name, freePct, vg.Size)
	if freePct <= vgWarnFreePct {
		return Result{
			"volume-group", Warn, detail,
			"low headroom for new pools/thick LVs — add a disk (vgextend) or free space",
		}
	}
	return Result{"volume-group", Pass, detail, ""}
}

func checkThinPools(pools []ThinPoolUsage) Result {
	if len(pools) == 0 {
		return Result{"thin-pools", Pass, "no thin pools (thick LVs only)", ""}
	}
	var fail, warn []string
	for _, p := range pools {
		switch worst := max(p.DataPercent, p.MetaPercent); {
		case worst >= thinFailPercent:
			fail = append(fail, fmt.Sprintf("%s data=%.1f%% meta=%.1f%%", p.Name, p.DataPercent, p.MetaPercent))
		case worst >= thinWarnPercent:
			warn = append(warn, fmt.Sprintf("%s data=%.1f%% meta=%.1f%%", p.Name, p.DataPercent, p.MetaPercent))
		}
	}
	hint := "reclaim space (delete/shrink volumes, prune snapshots) or vgextend and lvextend the pool before it exhausts"
	if len(fail) > 0 {
		return Result{"thin-pools", Fail, "near-full: " + strings.Join(fail, "; "), hint}
	}
	if len(warn) > 0 {
		return Result{"thin-pools", Warn, "filling: " + strings.Join(warn, "; "), hint}
	}
	names := make([]string, len(pools))
	for i, p := range pools {
		names[i] = p.Name
	}
	sort.Strings(names)
	return Result{"thin-pools", Pass, fmt.Sprintf("%d pool(s) healthy: %s", len(pools), strings.Join(names, ",")), ""}
}

// checkSystemMirror checks both halves of the system volume's local redundancy
// (ARCHITECTURE.md §3.4): raidProfile != "" (R8) is checked first because it is
// the more urgent mistake — RAID5/6 has an unfixed write hole and is never offered
// by any disko layout, so seeing it means someone reconfigured the volume by hand.
func checkSystemMirror(devices int, raidProfile string, md *MDArray, errStr string) Result {
	if errStr != "" || devices == 0 {
		return Result{"system-mirror", Warn, "could not determine: " + errStr, "check `btrfs filesystem show`/`df` on the system volume"}
	}
	if raidProfile != "" {
		return Result{
			"system-mirror", Fail, "using btrfs " + raidProfile,
			"btrfs RAID5/6 has an unfixed write hole — never use it (ARCHITECTURE.md §3.2); recreate as RAID1 or single",
		}
	}
	if devices == 1 && md != nil {
		return checkMDMirror(*md)
	}
	if devices == 1 {
		return Result{
			"system-mirror", Warn, "single device",
			"btrfs can detect corruption here but not repair it — mirror the system volume (ARCHITECTURE.md §3.4)",
		}
	}
	return Result{"system-mirror", Pass, fmt.Sprintf("mirrored across %d devices", devices), ""}
}

// MDArray is an md RAID array's state from sysfs.
type MDArray struct {
	Name      string // e.g. "md127"
	Level     string // e.g. "raid1"
	RaidDisks int
	Degraded  int // members missing
}

// checkMDMirror judges a system volume mirrored by md RAID1: btrfs detects corruption, md keeps a second copy.
func checkMDMirror(md MDArray) Result {
	if md.Level != "raid1" {
		return Result{"system-mirror", Warn, fmt.Sprintf("on md %s (%s), not a mirror", md.Name, md.Level), "the mirror layout uses md RAID1 (docs/STORAGE.md §4)"}
	}
	if md.Degraded > 0 {
		return Result{
			"system-mirror", Warn, fmt.Sprintf("md %s RAID1 degraded: %d of %d members missing", md.Name, md.Degraded, md.RaidDisks),
			"replace the failed disk and re-add its partitions (docs/STORAGE.md §4, replacing a system disk)",
		}
	}
	return Result{"system-mirror", Pass, fmt.Sprintf("md %s RAID1 across %d devices", md.Name, md.RaidDisks), ""}
}

func checkDRBDResources(res []drbd.Status, errStr string) Result {
	if errStr != "" {
		return Result{"drbd-resources", Warn, errStr, "check `drbdsetup status --json`"}
	}
	if len(res) == 0 {
		return Result{"drbd-resources", Warn, "no resources configured", "expected if no volumes have been created yet"}
	}
	var bad []string
	for _, r := range res {
		for _, v := range r.Volumes {
			switch {
			case !v.Quorum:
				bad = append(bad, fmt.Sprintf("%s/%d no quorum", r.Name, v.Number))
			case v.DiskState != drbd.DiskUpToDate:
				bad = append(bad, fmt.Sprintf("%s/%d disk %s", r.Name, v.Number, v.DiskState))
			}
		}
		for _, p := range r.Peers {
			if p.Connection == drbd.ConnStandAlone {
				bad = append(bad, fmt.Sprintf("%s peer %s standalone (disconnected, or split-brain — check drbdadm cstate)", r.Name, p.Name))
			}
		}
	}
	if len(bad) > 0 {
		return Result{
			"drbd-resources", Fail, strings.Join(bad, "; "),
			"a replica is degraded or unreachable — check `drbdsetup status --json --verbose` on every node",
		}
	}
	return Result{"drbd-resources", Pass, fmt.Sprintf("%d resource(s) healthy", len(res)), ""}
}

// CollectStorageLive fills StorageInput for the given VG. vg == "" skips the
// VG/pool checks (degrading them to WARN) — a node with no volume storage
// configured. It never returns an error: rows that cannot run are surfaced
// via the checks' WARN/FAIL paths instead.
func CollectStorageLive(ctx context.Context, vg, systemMount string) StorageInput {
	in := StorageInput{VGRequested: vg != ""}
	in.ModuleLoaded, in.ModuleVersion = collectDRBDModule()

	if vg != "" {
		l := lvm.New()
		if v, err := l.VG(ctx, vg); err != nil {
			in.VGErr = err.Error()
		} else {
			in.VG = &v
			if lvs, err := l.List(ctx, vg); err != nil {
				in.VGErr = err.Error()
			} else {
				for _, lv := range lvs {
					if lv.Type == lvm.ThinPool {
						in.Pools = append(in.Pools, ThinPoolUsage{Name: lv.Name, DataPercent: lv.DataPercent, MetaPercent: lv.MetaPercent})
					}
				}
			}
		}
	}

	in.SystemDevices, in.SystemErr = collectSystemDevices(systemMount)
	in.SystemMD = collectSystemMD(systemMount)
	in.SystemRAIDProfile = collectSystemRAIDProfile(systemMount)

	if res, err := drbd.New().StatusAll(ctx); err != nil {
		in.ResourcesErr = err.Error()
	} else {
		in.Resources = res
	}
	return in
}

// collectDRBDModule reads the loaded module's version straight from sysfs —
// present only while the module is actually loaded, not merely installed.
func collectDRBDModule() (loaded bool, version string) {
	data, err := os.ReadFile("/sys/module/drbd/version")
	if err != nil {
		return false, ""
	}
	return true, strings.TrimSpace(string(data))
}

// collectSystemDevices counts the block devices backing the btrfs filesystem
// mounted at mount (1 = unmirrored).
func collectSystemDevices(mount string) (int, string) {
	out, err := runCheck("btrfs", "filesystem", "show", mount)
	if err != nil {
		return 0, firstLine(out)
	}
	return strings.Count(out, "devid"), ""
}

// btrfsDevicePaths lists the member device paths in `btrfs filesystem show` output.
func btrfsDevicePaths(out string) []string {
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) >= 3 && f[0] == "devid" && f[len(f)-2] == "path" {
			paths = append(paths, f[len(f)-1])
		}
	}
	return paths
}

// collectSystemMD returns the md array a single-device system btrfs sits on, or nil.
func collectSystemMD(mount string) *MDArray {
	out, err := runCheck("btrfs", "filesystem", "show", mount)
	if err != nil {
		return nil
	}
	paths := btrfsDevicePaths(out)
	if len(paths) != 1 {
		return nil
	}
	dev, err := filepath.EvalSymlinks(paths[0])
	if err != nil {
		return nil
	}
	md, err := readMDArray("/sys/block", filepath.Base(dev))
	if err != nil {
		return nil
	}
	return md
}

// readMDArray reads an md array's level, member count and degraded count from sysBlock/<name>/md.
func readMDArray(sysBlock, name string) (*MDArray, error) {
	dir := filepath.Join(sysBlock, name, "md")
	read := func(f string) (string, error) {
		b, err := os.ReadFile(filepath.Join(dir, f))
		return strings.TrimSpace(string(b)), err
	}
	md := &MDArray{Name: name}
	var err error
	if md.Level, err = read("level"); err != nil {
		return nil, err
	}
	for f, dst := range map[string]*int{"raid_disks": &md.RaidDisks, "degraded": &md.Degraded} {
		v, err := read(f)
		if err != nil {
			return nil, err
		}
		if *dst, err = strconv.Atoi(v); err != nil {
			return nil, fmt.Errorf("md %s %s: %w", name, f, err)
		}
	}
	return md, nil
}

// collectSystemRAIDProfile reports "RAID5"/"RAID6" if either is in use on the
// btrfs filesystem mounted at mount (R8), "" otherwise (including on error —
// this is a best-effort second opinion, checkSystemMirror already covers the
// device-count case when it cannot run at all).
func collectSystemRAIDProfile(mount string) string {
	out, err := runCheck("btrfs", "filesystem", "df", mount)
	if err != nil {
		return ""
	}
	switch upper := strings.ToUpper(out); {
	case strings.Contains(upper, "RAID6"):
		return "RAID6"
	case strings.Contains(upper, "RAID5"):
		return "RAID5"
	}
	return ""
}
