package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/expanse/expanse/internal/blocks/vm/qmp"
	"github.com/expanse/expanse/internal/blocks/vm/vmstate"
)

// stopReplica is the unit's ExecStop, run before systemd signals the unit's processes: it saves a
// suspended VM's state, or asks the guest to power off and waits, so qemu isn't killed under it.
func stopReplica(instance string) {
	s, err := loadSpec(instance)
	if err != nil || s.Type != "vm/instance" {
		return
	}
	pid, err := strconv.Atoi(os.Getenv("MAINPID")) // empty once the main process has exited
	if err != nil || pid <= 0 {
		return
	}
	if s.Stopped && s.StateToken != "" {
		err := suspendVM(instance, s.StateToken)
		if err == nil {
			waitGone(pid, quitWait, 250*time.Millisecond)
			return
		}
		fmt.Fprintf(os.Stderr, "expanse-block-run: vm suspend failed, powering off instead: %v\n", err)
	}
	wait := shutdownWait(s)
	if wait == 0 {
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

// saveTimeout bounds writing a guest's RAM to disk; the unit's TimeoutStopSec leaves room for it.
const saveTimeout = 8 * time.Minute

// quitWait is how long qemu gets to exit once told to quit.
const quitWait = 30 * time.Second

// suspendVM saves the guest's state under token, then ends qemu. On failure the guest is running.
func suspendVM(instance, token string) error {
	sock, dir := qmp.SocketPath(instance), vmstate.Dir("", instance)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = os.Remove(vmstate.TempPath(dir))
	if err := qmp.Save(sock, vmstate.TempPath(dir), saveTimeout); err != nil {
		_ = vmstate.Remove(dir)
		return err
	}
	if err := vmstate.Commit(dir, token); err != nil {
		_ = vmstate.Remove(dir)
		_ = qmp.Cont(sock)
		return err
	}
	fmt.Printf("expanse-block-run: vm %s suspended (%s)\n", instance, token)
	qmp.Quit(sock)
	return nil
}

// resumeOrDiscard reports whether dir holds the save token names; any other save is deleted,
// since the disk moves on once the guest boots without it.
func resumeOrDiscard(dir, token string) bool {
	if token != "" && vmstate.Token(dir) == token {
		return true
	}
	_ = vmstate.Remove(dir)
	return false
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
