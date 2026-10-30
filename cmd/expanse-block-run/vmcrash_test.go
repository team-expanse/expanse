package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/blocks/vm"
)

func TestAGuestThatStopsSoonAfterBootCrashed(t *testing.T) {
	for _, c := range []struct {
		uptime   time.Duration
		stopping bool
		want     bool
	}{
		{3 * time.Second, false, true},
		{vm.CrashWindow - time.Second, false, true},
		{vm.CrashWindow, false, false}, // ran long enough: a reboot or power-off from inside
		{3 * time.Second, true, false}, // its unit's stop powered it off
	} {
		err := guestExit(c.uptime, func() bool { return c.stopping })
		if got := errors.Is(err, errGuestCrashed); got != c.want {
			t.Errorf("uptime %v stopping %v: crashed = %v, want %v", c.uptime, c.stopping, got, c.want)
		}
	}
}

func TestACrashedGuestExitsWithTheCrashStatus(t *testing.T) {
	if got := vmExitStatus(fmt.Errorf("vm/instance: %w", errGuestCrashed)); got != vm.CrashExitStatus {
		t.Errorf("crash exit status = %d, want %d", got, vm.CrashExitStatus)
	}
	if got := vmExitStatus(errors.New("no bound storage volume yet")); got != 1 {
		t.Errorf("other failure exit status = %d, want 1", got)
	}
}

func TestUnitStoppingReadsDeactivating(t *testing.T) {
	if !unitStoppingFrom("deactivating\n") || unitStoppingFrom("active\n") {
		t.Error("unitStoppingFrom: want only deactivating to count as stopping")
	}
}
