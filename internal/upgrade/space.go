package upgrade

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// BuildReserve is the room a build needs beyond what it copies from the release cache.
const BuildReserve = 256 << 20

// btrfsMetadataReserve is the metadata room a build needs: it writes tens of thousands of small files.
const btrfsMetadataReserve = 256 << 20

// btrfsFSInfo is BTRFS_IOC_FS_INFO, and btrfsFSInfoSize its argument's size; the fsid is at offset 16.
const (
	btrfsFSInfo     = 0x8400941f
	btrfsFSInfoSize = 1024
)

// FreeFunc reports the bytes free on the filesystem holding path.
type FreeFunc func(path string) (uint64, error)

func statfsFree(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// needSpace refuses what when path's filesystem has less than need free. A check that cannot run
// lets the upgrade go on, to fail with the real error if there is one.
func needSpace(free FreeFunc, path, what string, need uint64) error {
	if free == nil {
		free = statfsFree
		if dir, ok := btrfsSysfsDir(path); ok {
			if room, ok := btrfsMetadataRoom(dir); ok && room < btrfsMetadataReserve {
				return metadataError(path, what, room)
			}
		}
	}
	got, err := free(path)
	if err != nil {
		return nil //nolint:nilerr // the upgrade itself reports a filesystem it cannot use
	}
	if got >= need {
		return nil
	}
	return fmt.Errorf("not enough disk space for %s: it needs %s free in %s, and %s is free",
		what, gigabytes(need), path, gigabytes(got))
}

func gigabytes(n uint64) string { return fmt.Sprintf("%.1f GB", float64(n)/1e9) }

func metadataError(path, what string, room uint64) error {
	return fmt.Errorf("not enough btrfs metadata space for %s in %s: %d MiB can be used and it needs %d MiB, "+
		"even if df shows more free; `btrfs balance start -dusage=50 %s` returns unused data space",
		what, path, room>>20, btrfsMetadataReserve>>20, path)
}

// btrfsSysfsDir finds path's /sys/fs/btrfs/<uuid>, when path is on btrfs.
func btrfsSysfsDir(path string) (string, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil || st.Type != unix.BTRFS_SUPER_MAGIC {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	var info [btrfsFSInfoSize]byte
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), btrfsFSInfo, uintptr(unsafe.Pointer(&info))); errno != 0 {
		return "", false
	}
	id := info[16:32]
	return fmt.Sprintf("/sys/fs/btrfs/%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), true
}

// btrfsMetadataRoom is how many bytes of metadata the filesystem can still write: what its metadata
// chunks hold free, plus what new chunks could add. df counts neither limit.
func btrfsMetadataRoom(dir string) (uint64, bool) {
	var total, used, disk, reserve, data, system uint64
	for file, into := range map[string]*uint64{
		"allocation/metadata/total_bytes": &total,
		"allocation/metadata/bytes_used":  &used,
		"allocation/metadata/disk_total":  &disk,
		"allocation/global_rsv_size":      &reserve,
		"allocation/data/disk_total":      &data,
		"allocation/system/disk_total":    &system,
	} {
		v, ok := readUint(filepath.Join(dir, file))
		if !ok {
			return 0, false
		}
		*into = v
	}
	devices, _ := filepath.Glob(filepath.Join(dir, "devices", "*", "size"))
	var size uint64
	for _, d := range devices {
		sectors, ok := readUint(d)
		if !ok {
			return 0, false
		}
		size += sectors * 512
	}
	if size == 0 || total == 0 {
		return 0, false
	}
	var room uint64
	if total > used+reserve {
		room = total - used - reserve
	}
	if allocated := data + disk + system; size > allocated {
		room += (size - allocated) / max(disk/total, 1) // DUP and RAID1 write metadata twice
	}
	return room, true
}

func readUint(file string) (uint64, bool) {
	b, err := os.ReadFile(file)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return v, err == nil
}

// missingNarSize totals the unpacked size of the cache's store paths that store does not hold yet.
func missingNarSize(cache, store string) uint64 {
	infos, _ := filepath.Glob(filepath.Join(cache, "*.narinfo"))
	var total uint64
	for _, info := range infos {
		path, size := readNarInfo(info)
		if path == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(store, filepath.Base(path))); os.IsNotExist(err) {
			total += size
		}
	}
	return total
}

func readNarInfo(file string) (path string, size uint64) {
	f, err := os.Open(file)
	if err != nil {
		return "", 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), ": ")
		switch k {
		case "StorePath":
			path = v
		case "NarSize":
			size, _ = strconv.ParseUint(v, 10, 64)
		}
	}
	return path, size
}
