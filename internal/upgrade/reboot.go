package upgrade

import "path/filepath"

// RebootNeeded reports whether system's kernel, initrd or kernel modules differ from booted's, the
// rule NixOS's system.autoUpgrade.allowReboot uses. No booted system (not NixOS) needs none.
func RebootNeeded(booted, system string) bool {
	if booted == "" {
		return false
	}
	for _, name := range []string{"kernel", "initrd", "kernel-modules"} {
		a, errA := filepath.EvalSymlinks(filepath.Join(booted, name))
		b, errB := filepath.EvalSymlinks(filepath.Join(system, name))
		if errA != nil || errB != nil || a != b {
			return true
		}
	}
	return false
}
