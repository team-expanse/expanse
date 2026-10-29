package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/expanse/expanse/internal/blocks/vm/qmp"
)

// stopReplica is the unit's ExecStop, run before systemd signals the unit's processes:
// it asks a VM's guest to power off and waits for it, so qemu isn't killed under a running guest.
func stopReplica(instance string) {
	s, err := loadSpec(instance)
	if err != nil {
		return
	}
	wait := shutdownWait(s)
	if wait == 0 {
		return
	}
	pid, err := strconv.Atoi(os.Getenv("MAINPID")) // empty once the main process has exited
	if err != nil || pid <= 0 {
		return
	}
	if err := qmp.Powerdown(qmp.SocketPath(instance)); err != nil {
		fmt.Fprintf(os.Stderr, "expanse-block-run: vm power-off request failed: %v\n", err)
		return
	}
	if !waitGone(pid, wait, 250*time.Millisecond) {
		fmt.Fprintf(os.Stderr, "expanse-block-run: the guest did not power off within %v; stopping qemu\n", wait)
	}
}

// shutdownWait is how long a stop waits for the guest to power off; 0 for non-VMs.
func shutdownWait(s *spec) time.Duration {
	if s.Type != "vm/instance" {
		return 0
	}
	cfg, err := cfgMap(s.Args)
	if err != nil {
		return 0
	}
	d, err := vmShutdownTimeout(cfg)
	if err != nil {
		return 0
	}
	return d
}

// waitGone polls until pid has exited or timeout passes, and reports whether it exited.
func waitGone(pid int, timeout, poll time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(poll)
	}
}
