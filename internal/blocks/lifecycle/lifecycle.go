// Package lifecycle implements the block lifecycle state machine
// (PHASE04.md §5.1). This card is the PURE state machine: Transition is a
// table lookup with no side effects and no I/O. The side effects listed in
// the spec's table (write placements, build nix closures, systemd units,
// LB pools, ...) are implemented by the controller/runtime cards that call
// Transition — they own doing, this package owns deciding.
package lifecycle

import (
	"fmt"

	pb "github.com/expanse/expanse/proto"

	"github.com/expanse/expanse/internal/errors"
)

// Phase is the block/replica lifecycle phase (§5.1).
type Phase = pb.Phase

// The full set of states in the §5.1 table.
const (
	Pending      = pb.Phase_PENDING
	Scheduling   = pb.Phase_SCHEDULING
	Provisioning = pb.Phase_PROVISIONING
	Starting     = pb.Phase_STARTING
	Running      = pb.Phase_RUNNING
	Degraded     = pb.Phase_DEGRADED
	Failed       = pb.Phase_FAILED
	Updating     = pb.Phase_UPDATING
	Terminating  = pb.Phase_TERMINATING
	Terminated   = pb.Phase_TERMINATED
)

// Event is a lifecycle transition trigger (§5.1 "Event" column).
type Event string

const (
	EventCreate        Event = "create"
	EventScheduled     Event = "scheduled"
	EventNodeAccepted  Event = "node-accepted"
	EventUnitStarted   Event = "unit-started"
	EventReadinessPass Event = "readiness-pass"
	EventReadinessFail Event = "readiness-fail"
	EventLivenessFail  Event = "liveness-fail"
	EventSpecChange    Event = "spec-change"
	EventUpdateDone    Event = "update-done"
	EventUpdateFail    Event = "update-fail"
	EventDelete        Event = "delete"
	EventCleanupDone   Event = "cleanup-done"
	EventBackoffUp     Event = "backoff-elapsed"
)

// Side effects per accepted transition (§5.1 table, last column). These are
// descriptions for the cards that perform them, not executable code.
var sideEffects = map[string]string{
	key(0, EventCreate):                 "validate, persist spec, create generation",
	key(Pending, EventScheduled):        "write placements",
	key(Scheduling, EventNodeAccepted):  "agent builds nix closure, creates volumes",
	key(Provisioning, EventUnitStarted): "systemd unit active",
	key(Starting, EventReadinessPass):   "add to LB pool, announce DNS",
	key(Running, EventReadinessFail):    "remove from LB pool",
	key(Degraded, EventReadinessPass):   "re-add to LB",
	key(Degraded, EventLivenessFail):    "restart or reschedule",
	key(Running, EventSpecChange):       "rolling update",
	key(Updating, EventUpdateDone):      "",
	key(Updating, EventUpdateFail):      "auto-rollback if configured",
	key(-1, EventDelete):                "stop units, release resources",
	key(Terminating, EventCleanupDone):  "remove from store",
	key(Failed, EventBackoffUp):         "retry with backoff",
}

func key(from pb.Phase, ev Event) string {
	return fmt.Sprintf("%d/%s", from, ev)
}

// transitions is the §5.1 table, from → event → to.
var transitions = map[pb.Phase]map[Event]pb.Phase{
	0:            {EventCreate: Pending}, // "—" = the not-yet-created block
	Pending:      {EventScheduled: Scheduling},
	Scheduling:   {EventNodeAccepted: Provisioning},
	Provisioning: {EventUnitStarted: Starting},
	Starting:     {EventReadinessPass: Running},
	Running: {
		EventReadinessFail: Degraded,
		EventSpecChange:    Updating,
		EventDelete:        Terminating,
	},
	Degraded: {
		EventReadinessPass: Running,
		EventLivenessFail:  Failed,
		EventDelete:        Terminating,
	},
	Updating: {
		EventUpdateDone: Running,
		EventUpdateFail: Degraded,
		EventDelete:     Terminating,
	},
	Terminating: {EventCleanupDone: Terminated},
	Failed: {
		EventBackoffUp: Scheduling,
		EventDelete:    Terminating,
	},
}

// isLive reports whether from is a created-but-not-dead block phase —
// the "any → Terminating" rows of the table.
func isLive(from Phase) bool {
	switch from {
	case Pending, Scheduling, Provisioning, Starting, Running, Degraded, Failed, Updating:
		return true
	}
	return false
}

// Transition returns the target phase for (from, event) and whether the
// transition is legal. Illegal transitions return ok=false and leave the
// caller's state untouched — never guess a phase.
func Transition(from Phase, ev Event) (to Phase, ok bool) {
	// "any → delete → Terminating" applies to every live phase.
	if ev == EventDelete && isLive(from) {
		return Terminating, true
	}
func Transition(from Phase, ev Event) (to Phase, ok bool) {
	row, ok := transitions[from]
	if !ok {
		return pb.Phase_PHASE_UNSPECIFIED, false
	}
	to, ok = row[ev]
	if !ok {
		return pb.Phase_PHASE_UNSPECIFIED, false
	}
	return to, true
}

// MustTransition applies Transition and returns a typed error on an illegal
// transition — for callers that have already gated the event.
func MustTransition(from Phase, ev Event) (Phase, error) {
	to, ok := Transition(from, ev)
	if !ok {
		return from, errors.New(errors.KindConflict, "lifecycle.Transition",
			fmt.Sprintf("illegal transition %s --%s--> ? (from phase %s)", from, ev, from))
	}
	return to, nil
}

// SideEffect describes what performing this transition entails (§5.1 last
// column). Empty string for no side effects.
func SideEffect(from Phase, ev Event) string {
	if ev == EventDelete && from != 0 {
		return sideEffects[key(-1, EventDelete)] // "any" row
	}
	return sideEffects[key(from, ev)]
}

// DeriveBlockPhase aggregates replica phases into the block-level phase
// (§5.1, verbatim rules):
//
//   - all replicas Running        → Running
//   - some Running, some not      → Degraded
//   - none Running, some Pending  → Pending
//   - none Running, all Failed    → Failed
//
// An empty replica list has no replicas running and none pending/failed;
// nothing is observable yet, so the block is Pending (nothing scheduled).
// Terminated blocks are reported by their own phase through single-replica
// aggregation below.
func DeriveBlockPhase(replicaPhases []Phase) Phase {
	if len(replicaPhases) == 0 {
		return Pending
	}
	running, pending, failed := 0, 0, 0
	for _, p := range replicaPhases {
		switch p {
		case Running:
			running++
		case Pending:
			pending++
		case Failed:
			failed++
		}
	}
	switch {
	case running == len(replicaPhases):
		return Running
	case running > 0:
		return Degraded
	case pending > 0:
		return Pending
	case failed == len(replicaPhases):
		return Failed
	default:
		// Mixed non-running non-pending non-failed states (scheduling,
		// provisioning, starting, degraded, updating...): nothing Running
		// and nothing Pending → the block has not reached Running.
		return Degraded
	}
}
