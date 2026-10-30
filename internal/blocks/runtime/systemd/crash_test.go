package systemd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/blocks/vm"
	"github.com/expanse/expanse/internal/reconcile"
)

func crashSpec() Spec {
	s := sampleSpec()
	s.Type, s.RunAsRoot = "vm/instance", true
	return s
}

// crashTest drives one VM replica on a fake unit API and a settable clock.
type crashTest struct {
	t    *testing.T
	api  *fakeUnitAPI
	m    *Manager
	res  reconcile.Resource
	unit string
	now  time.Time
}

func newCrashTest(t *testing.T) *crashTest {
	api := &fakeUnitAPI{states: map[string][3]string{}, status: map[string]string{}, exits: map[string]int{}}
	m, res := converged(t, api, crashSpec())
	c := &crashTest{t: t, api: api, m: m, res: res, unit: UnitNameForSpec(crashSpec()), now: time.Unix(1000, 0)}
	m.now = func() time.Time { return c.now }
	return c
}

// crash has the guest exit as a boot crash does.
func (c *crashTest) crash(exit int) {
	c.api.states[c.unit] = [3]string{"loaded", "failed", "failed"}
	c.api.exits[c.unit] = exit
}

// tick runs one reconcile pass: it reports the observed status and whether the unit started.
func (c *crashTest) tick() (status string, started bool, planErr error) {
	c.t.Helper()
	ctx := context.Background()
	o, err := c.m.Observe(ctx, c.res)
	if err != nil {
		c.t.Fatal(err)
	}
	before := c.api.starts
	acts, err := c.m.Plan(ctx, c.res, o)
	for _, a := range acts {
		if err := c.m.Apply(ctx, a); err != nil {
			c.t.Fatal(err)
		}
	}
	return o.Details["status"], c.api.starts > before, err
}

func TestManagerBacksOffAGuestThatCrashesAtBoot(t *testing.T) {
	c := newCrashTest(t)
	waits := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second}
	for n, wait := range waits {
		c.crash(vm.CrashExitStatus)
		status, started, err := c.tick()
		if started || err == nil {
			t.Fatalf("crash %d: started=%v err=%v, want a wait", n+1, started, err)
		}
		if !strings.Contains(status, "crashed") || !strings.Contains(err.Error(), "crashed") {
			t.Fatalf("crash %d: status %q, err %v; want both to say the guest crashed", n+1, status, err)
		}
		c.now = c.now.Add(wait - time.Second)
		if _, started, _ := c.tick(); started {
			t.Fatalf("crash %d: started %v after the crash, before its %v wait", n+1, wait-time.Second, wait)
		}
		c.now = c.now.Add(time.Second)
		if _, started, err := c.tick(); !started || err != nil {
			t.Fatalf("crash %d: started=%v err=%v once the %v wait passed", n+1, started, err, wait)
		}
	}
}

func TestManagerCapsTheCrashWait(t *testing.T) {
	if got := crashWait(20); got != maxCrashWait {
		t.Fatalf("crashWait(20) = %v, want %v", got, maxCrashWait)
	}
}

func TestManagerForgetsCrashesOnceTheGuestKeepsRunning(t *testing.T) {
	c := newCrashTest(t)
	c.crash(vm.CrashExitStatus)
	c.tick()
	c.now = c.now.Add(10 * time.Second)
	c.tick() // starts it again
	c.now = c.now.Add(vm.CrashWindow)
	if status, _, err := c.tick(); err != nil || strings.Contains(status, "crashed") {
		t.Fatalf("a running guest: status %q err %v", status, err)
	}
	c.crash(vm.CrashExitStatus)
	c.tick()
	c.now = c.now.Add(10 * time.Second)
	if _, started, _ := c.tick(); !started {
		t.Fatal("a crash after a good run waited longer than the first wait")
	}
}

func TestManagerStartsOtherFailuresAtOnce(t *testing.T) {
	c := newCrashTest(t)
	c.crash(1)
	if _, started, err := c.tick(); !started || err != nil {
		t.Fatalf("a non-crash failure: started=%v err=%v, want an immediate start", started, err)
	}
}
