package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/blocks/vm/vmstate"
)

func TestShutdownWaitIsOnlyForVMs(t *testing.T) {
	cases := []struct {
		s    spec
		want time.Duration
	}{
		{spec{Type: "share/nfs"}, 0},
		{spec{Type: "vm/instance"}, time.Minute},
		{spec{Type: "vm/instance", Args: []string{"--config", `{"shutdownTimeout":5}`}}, 5 * time.Second},
		{spec{Type: "vm/instance", Args: []string{"--config", `{"shutdownTimeout":0}`}}, 0},
	}
	for _, c := range cases {
		if got := shutdownWait(&c.s); got != c.want {
			t.Errorf("shutdownWait(%s %q) = %v, want %v", c.s.Type, c.s.Args, got, c.want)
		}
	}
}

func TestWaitGoneReturnsWhenTheProcessExits(t *testing.T) {
	cmd := exec.Command("sleep", "0.3")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait() //nolint:errcheck // reaps the child so its /proc entry goes away
	if !waitGone(cmd.Process.Pid, 5*time.Second, 50*time.Millisecond) {
		t.Error("waitGone gave up on a process that exited")
	}
}

func TestWaitGoneGivesUpAtTheDeadline(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	t0 := time.Now()
	if waitGone(cmd.Process.Pid, 200*time.Millisecond, 50*time.Millisecond) {
		t.Error("waitGone reported a running process gone")
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Errorf("waitGone took %v past a 200ms deadline", d)
	}
}

// A guest still booting misses the first presses; the later ones power it off.
func TestPowerOffPressesAgainUntilTheGuestExits(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait() //nolint:errcheck // reaps the child so its /proc entry goes away
	presses := 0
	press := func() error {
		if presses++; presses == 3 {
			_ = cmd.Process.Kill()
		}
		return nil
	}
	if gone, err := powerOff(cmd.Process.Pid, press, 5*time.Second, 100*time.Millisecond, 20*time.Millisecond); err != nil || !gone {
		t.Fatalf("powerOff = %v, %v; want the guest gone", gone, err)
	}
	if presses != 3 {
		t.Errorf("pressed %d times, want 3", presses)
	}
}

func TestPowerOffGivesUpAtTheDeadline(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	presses := 0
	press := func() error { presses++; return nil }
	if gone, _ := powerOff(cmd.Process.Pid, press, 350*time.Millisecond, 100*time.Millisecond, 20*time.Millisecond); gone {
		t.Error("powerOff reported a running guest gone")
	}
	if presses < 3 || presses > 5 {
		t.Errorf("pressed %d times in 350ms at 100ms apart, want about 4", presses)
	}
}

// A first press that fails means no monitor to ask; later ones may fail as qemu exits.
func TestPowerOffReportsOnlyTheFirstPressFailing(t *testing.T) {
	presses := 0
	press := func() error { presses++; return errors.New("no monitor") }
	if _, err := powerOff(os.Getpid(), press, time.Second, 10*time.Millisecond, 5*time.Millisecond); err == nil {
		t.Error("a failed first press was not reported")
	}
	if presses != 1 {
		t.Errorf("pressed %d times after the monitor failed, want 1", presses)
	}
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait() //nolint:errcheck // reaps the child so its /proc entry goes away
	presses = 0
	press = func() error {
		if presses++; presses == 1 {
			return nil
		}
		_ = cmd.Process.Kill()
		return errors.New("qemu is exiting")
	}
	if gone, err := powerOff(cmd.Process.Pid, press, 5*time.Second, 50*time.Millisecond, 10*time.Millisecond); err != nil || !gone {
		t.Errorf("powerOff = %v, %v; a later failed press should not end the wait", gone, err)
	}
}

func savedAs(t *testing.T, token string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "default-vm-0")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vmstate.TempPath(dir), []byte("ram"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := vmstate.Commit(dir, token); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAStartResumesOnlyTheSaveItsSpecNames(t *testing.T) {
	dir := savedAs(t, "r5")
	if !resumeOrDiscard(dir, "r5") {
		t.Fatal("the named save was not resumed")
	}
	if vmstate.Token(dir) != "r5" {
		t.Error("the save to resume from was deleted before loading")
	}
}

func TestAColdBootDeletesAnyOtherSave(t *testing.T) {
	for _, token := range []string{"", "r6"} {
		dir := savedAs(t, "r5")
		if resumeOrDiscard(dir, token) {
			t.Errorf("token %q resumed the r5 save", token)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("token %q: the stale save survived a cold boot", token)
		}
	}
}
