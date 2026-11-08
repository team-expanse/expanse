package upgrade

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// BuildReserve is the free space a build needs in the Nix store beyond the release's downloads.
const BuildReserve = 1 << 30

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
