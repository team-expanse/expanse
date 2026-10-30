package systemd

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// MountPoint reports whether path is a mount point in this process's mount namespace.
func MountPoint(path string) (bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	return mountPointIn(f, path)
}

// mountPointIn reports whether path is a mount point in a mountinfo listing.
func mountPointIn(r io.Reader, path string) (bool, error) {
	want := filepath.Clean(path)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) > 4 && unescapeMountinfo(f[4]) == want {
			return true, nil
		}
	}
	return false, sc.Err()
}

// unescapeMountinfo decodes the kernel's octal escapes (\040 for a space).
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// missingMount is the first of a replica's volume mounts that isn't mounted yet, or "".
func (m *Manager) missingMount(s Spec) (string, error) {
	if m.Mounted == nil || s.Stopped {
		return "", nil
	}
	for _, p := range s.VolumeMounts {
		ok, err := m.Mounted(p)
		if err != nil {
			return "", err
		}
		if !ok {
			return p, nil
		}
	}
	return "", nil
}
