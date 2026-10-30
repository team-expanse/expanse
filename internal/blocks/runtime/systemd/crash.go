package systemd

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/expanse/expanse/internal/blocks/vm"
)

// maxCrashWait caps the wait before a crashing guest's next start.
const maxCrashWait = 5 * time.Minute

// crashWait is the wait after a guest's n-th crash in a row: 10s, doubling, up to maxCrashWait.
func crashWait(n int) time.Duration {
	d := 10 * time.Second
	for i := 1; i < n && d < maxCrashWait; i++ {
		d *= 2
	}
	return min(d, maxCrashWait)
}

// crashes tracks the guests that crashed at boot, per unit.
type crashes struct {
	mu    sync.Mutex
	units map[string]*crashRun
}

type crashRun struct {
	count     int
	started   time.Time
	crashedAt time.Time
	counted   bool // this run's crash is in count
}

func (c *crashes) run(unit string) *crashRun {
	if c.units == nil {
		c.units = map[string]*crashRun{}
	}
	r := c.units[unit]
	if r == nil {
		r = &crashRun{}
		c.units[unit] = r
	}
	return r
}

// started records a start of unit's guest.
func (c *crashes) started(unit string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.run(unit)
	r.started, r.counted = now, false
}

// crashed counts unit's latest crash once and returns the wait left before its next start.
func (c *crashes) crashed(unit string, now time.Time) (n int, left time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.run(unit)
	if !r.counted {
		r.count, r.counted, r.crashedAt = r.count+1, true, now
	}
	return r.count, max(0, r.crashedAt.Add(crashWait(r.count)).Sub(now))
}

// running forgets unit's crashes once its guest has run past vm.CrashWindow.
func (c *crashes) running(unit string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.units[unit]; r != nil && now.Sub(r.started) >= vm.CrashWindow {
		delete(c.units, unit)
	}
}

// crashStatus reads a failed VM unit's crash: its status line and the wait left, "" when it didn't crash.
func (m *Manager) crashStatus(ctx context.Context, s Spec, unit, active string) (string, time.Duration) {
	if s.Type != "vm/instance" {
		return "", 0
	}
	now := m.clock()
	if active != "failed" {
		if active == "active" {
			m.crashes.running(unit, now)
		}
		return "", 0
	}
	if code, err := m.API.ExitStatus(ctx, unit); err != nil || code != vm.CrashExitStatus {
		return "", 0
	}
	n, left := m.crashes.crashed(unit, now)
	if left == 0 {
		return fmt.Sprintf("the guest crashed within %v of booting (%d in a row); starting it again", vm.CrashWindow, n), 0
	}
	return fmt.Sprintf("the guest crashed within %v of booting (%d in a row); next start in %v",
		vm.CrashWindow, n, left.Round(time.Second)), left
}

func (m *Manager) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}
