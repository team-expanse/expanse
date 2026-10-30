package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
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
	if why := keepReason(instance, s, "", systemStopping); why != "" {
		err := suspendVM(instance, vmstate.KeepToken(s.Placement))
		if err == nil {
			fmt.Printf("expanse-block-run: vm %s saved for %s; its next start resumes it\n", instance, why)
			waitGone(pid, quitWait, 250*time.Millisecond)
			return
		}
		fmt.Fprintf(os.Stderr, "expanse-block-run: vm save for %s failed, powering off instead: %v\n", why, err)
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
	if err := keepShape(instance, dir); err != nil {
		return err
	}
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

// keepShape copies the running layout beside the save; a VM from before hotplug has none to resume into.
func keepShape(instance, dir string) error {
	s, err := readShape(shapePath(instance))
	if err != nil {
		return fmt.Errorf("its layout is unknown, so a save could not be resumed: %w", err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return vmstate.WriteShape(dir, raw)
}

// resumeOrDiscard reports whether dir holds a save one of tokens names; any other save is deleted,
// since the disk moves on once the guest boots without it.
func resumeOrDiscard(dir string, tokens ...string) bool {
	if have := vmstate.Token(dir); have != "" && slices.Contains(tokens, have) {
		return true
	}
	_ = vmstate.Remove(dir)
	return false
}

// resumeFrom runs load and then deletes the save in dir, so it never loads twice; when load fails
// because qemu had already exited (it never read the save), the save is kept for the next start.
func resumeFrom(dir string, load func() error, exited <-chan struct{}) (kept bool, err error) {
	if err = load(); err != nil {
		select {
		case <-exited:
			return true, err
		default:
		}
	}
	_ = vmstate.Remove(dir)
	return false, err
}

// resumeTokens are the saves s may resume: its suspension's, and one kept for its placement.
func resumeTokens(s *spec) []string {
	tokens := []string{s.StateToken}
	if s.Placement != "" {
		tokens = append(tokens, vmstate.KeepToken(s.Placement))
	}
	return slices.DeleteFunc(tokens, func(t string) bool { return t == "" })
}

// keepReason says why this stop saves the running guest to resume it, "" for a power-off. It
// consumes the agent's keep request; a VM without a placement id can't be resumed safely.
func keepReason(instance string, s *spec, keepDir string, stopping func() bool) string {
	asked := os.Remove(vmstate.KeepPath(keepDir, instance)) == nil
	switch {
	case s.Type != "vm/instance" || s.Placement == "":
		return ""
	case asked:
		return "an update"
	case stopping():
		return "the node's shutdown"
	}
	return ""
}

// systemStopping reports whether the node is shutting down or rebooting.
func systemStopping() bool {
	out, _ := exec.Command("systemctl", "is-system-running").Output() // exits non-zero unless running
	return systemStoppingFrom(string(out))
}

func systemStoppingFrom(out string) bool { return strings.TrimSpace(out) == "stopping" }

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
