package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/blocks/vm"
)

// errGuestCrashed marks a guest that stopped on its own within vm.CrashWindow of booting.
var errGuestCrashed = errors.New("the guest stopped on its own soon after booting, so it counts as a crash")

// guestExit judges a guest that stopped on its own after uptime: a crash unless its unit is stopping it.
func guestExit(uptime time.Duration, stopping func() bool) error {
	if uptime >= vm.CrashWindow || stopping() {
		return nil
	}
	return fmt.Errorf("%w (%v after boot)", errGuestCrashed, uptime.Round(time.Second))
}

// vmExitStatus is the process exit status for runVM's error.
func vmExitStatus(err error) int {
	if errors.Is(err, errGuestCrashed) {
		return vm.CrashExitStatus
	}
	return 1
}

// ownUnitStopping reports whether systemd is stopping this process's unit (its ExecStop ran).
func ownUnitStopping() bool {
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return false
	}
	unit := unitFromCgroup(string(raw))
	path, err := resolveBin("systemctl")
	if unit == "" || err != nil {
		return false
	}
	out, _ := exec.Command(path, "show", "-P", "ActiveState", unit).Output()
	return unitStoppingFrom(string(out))
}

func unitStoppingFrom(out string) bool { return strings.TrimSpace(out) == "deactivating" }
