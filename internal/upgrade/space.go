package upgrade

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// BuildReserve is the room a build needs beyond what it copies from the release cache.
const BuildReserve = 256 << 20

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
