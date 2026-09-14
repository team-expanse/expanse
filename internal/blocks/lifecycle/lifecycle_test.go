package lifecycle

import (
	"testing"

	pb "github.com/expanse/expanse/proto"
)

// TestStateTable walks every row of the §5.1 table as a distinct case.
func TestStateTable(t *testing.T) {
	cases := []struct {
		from Phase
		ev   Event
		to   Phase
	}{
		{0, EventCreate, Pending},
		{Pending, EventScheduled, Scheduling},
		{Scheduling, EventNodeAccepted, Provisioning},
		{Provisioning, EventUnitStarted, Starting},
		{Starting, EventReadinessPass, Running},
		{Running, EventReadinessFail, Degraded},
		{Degraded, EventReadinessPass, Running},
		{Degraded, EventLivenessFail, Failed},
		{Running, EventSpecChange, Updating},
		{Updating, EventUpdateDone, Running},
		{Updating, EventUpdateFail, Degraded},
		{Running, EventDelete, Terminating},
		{Degraded, EventDelete, Terminating},
		{Updating, EventDelete, Terminating},
		{Failed, EventDelete, Terminating},
		{Pending, EventDelete, Terminating},
		{Terminating, EventCleanupDone, Terminated},
		{Failed, EventBackoffUp, Scheduling},
	}
	for _, c := range cases {
		got, ok := Transition(c.from, c.ev)
		if !ok || got != c.to {
			t.Errorf("Transition(%v, %v) = %v,%v; want %v,true", c.from, c.ev, got, ok, c.to)
		}
	}
}

// TestIllegalTransitions asserts rejections, incl. the spec-called-out
// Pending → Running directly.
func TestIllegalTransitions(t *testing.T) {
	cases := []struct {
		from Phase
		ev   Event
	}{
		{Pending, EventReadinessPass},   // Pending directly to Running
		{Pending, EventScheduled + "x"}, // unknown event
		{Running, EventCreate},          // create twice
		{Terminated, EventScheduled},    // dead block reschedules
		{Starting, EventUpdateDone},
	}
	for _, c := range cases {
		got, ok := Transition(c.from, c.ev)
		if ok {
			t.Errorf("Transition(%v, %v) accepted -> %v; want rejection", c.from, c.ev, got)
		}
		if got != pb.Phase_PHASE_UNSPECIFIED {
			t.Errorf("rejection returned phase %v, want UNSPECIFIED", got)
		}
	}
	if _, err := MustTransition(Pending, EventReadinessPass); err == nil {
		t.Error("MustTransition accepted an illegal transition")
	}
}

// TestSideEffects: every table row has a documented side effect ("" where
// the spec says "—").
func TestSideEffects(t *testing.T) {
	if s := SideEffect(Starting, EventReadinessPass); s == "" {
		t.Error("readiness-pass should document LB/DNS side effects")
	}
	if s := SideEffect(Updating, EventUpdateDone); s != "" {
		t.Errorf("update-done side effect = %q, want empty", s)
	}
	// "any → delete" resolves for every live phase.
	for _, from := range []Phase{Pending, Scheduling, Provisioning, Starting, Running, Degraded, Failed, Updating} {
		if _, ok := Transition(from, EventDelete); !ok {
			t.Errorf("delete from %v not legal", from)
		}
		if SideEffect(from, EventDelete) == "" {
			t.Errorf("delete from %v missing side effect", from)
		}
	}
}

// TestDeriveBlockPhase covers the four aggregation rules plus edges.
func TestDeriveBlockPhase(t *testing.T) {
	cases := []struct {
		name string
		in   []Phase
		want Phase
	}{
		{"all running", []Phase{Running, Running, Running}, Running},
		{"single running", []Phase{Running}, Running},
		{"some running some not", []Phase{Running, Starting, Degraded}, Degraded},
		{"some running some failed", []Phase{Running, Failed}, Degraded},
		{"none running some pending", []Phase{Pending, Scheduling, Pending}, Pending},
		{"all pending", []Phase{Pending}, Pending},
		{"none running all failed", []Phase{Failed, Failed}, Failed},
		{"single failed", []Phase{Failed}, Failed},
		{"empty", nil, Pending},
		{"provisioning only", []Phase{Provisioning, Provisioning}, Degraded},
	}
	for _, c := range cases {
		if got := DeriveBlockPhase(c.in); got != c.want {
			t.Errorf("%s: DeriveBlockPhase(%v) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}
