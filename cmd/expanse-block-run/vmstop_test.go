package main

import (
	"os/exec"
	"testing"
	"time"
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
