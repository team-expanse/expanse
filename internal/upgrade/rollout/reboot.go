package rollout

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// RebootState is a node reboot's.
type RebootState string

const (
	// RebootPending waits for a healthy cluster and no other reboot underway.
	RebootPending RebootState = "pending"
	// RebootDraining waits for the node's replicas to run elsewhere or be held.
	RebootDraining  RebootState = "draining"
	RebootRebooting RebootState = "rebooting"
	RebootDone      RebootState = "done"
	RebootFailed    RebootState = "failed"
	// RebootCancelled: stopped by the user before the node rebooted.
	RebootCancelled RebootState = "cancelled"
)

const (
	// DrainTimeout fails a reboot whose node still has replicas moving after this long.
	DrainTimeout = 15 * time.Minute
	// BackTimeout fails a reboot whose node has not reported a new boot after this long.
	BackTimeout = 15 * time.Minute
)

// Replica names one replica of one block.
type Replica struct {
	Block string `json:"block"` // namespace/name
	Index int32  `json:"index"`
}

func (r Replica) String() string { return fmt.Sprintf("%s replica %d", r.Block, r.Index) }

// Reboot is one node's drain and reboot, stored under RebootPrefix.
type Reboot struct {
	Node  string      `json:"node"`
	State RebootState `json:"state"`
	// InPlace: no other node can take its replicas, so it reboots without a drain.
	InPlace bool `json:"inPlace,omitempty"`
	// Cordoned: the node was cordoned before the drain, and stays so after it.
	Cordoned bool `json:"cordoned,omitempty"`
	// BootID is the boot the node was in when the reboot began; a new one means it rebooted.
	BootID string `json:"bootID,omitempty"`
	// Replicas were on the node when its drain began.
	Replicas []Replica `json:"replicas,omitempty"`
	// Held maps each replica that stays on the node through its reboot to why.
	Held   map[string]string `json:"held,omitempty"`
	Reason string            `json:"reason,omitempty"`
	// Cancel asks the leader to stop the reboot and put the node back in service.
	Cancel bool `json:"cancel,omitempty"`
	// Since is when State was entered; the timeouts count from it.
	Since   time.Time `json:"since"`
	Started time.Time `json:"started"`
	Updated time.Time `json:"updated"`
}

// Active reports whether the reboot is underway: begun, and not yet over.
func (r Reboot) Active() bool { return r.State == RebootDraining || r.State == RebootRebooting }

// Open reports whether the reboot still needs anything from the leader.
func (r Reboot) Open() bool { return r.State == RebootPending || r.Active() }

// NewReboot starts a pending reboot of node. A failed one before it may have left the node
// drained, so its record of the operator's cordon carries over.
func NewReboot(node string, prev Reboot, now time.Time) Reboot {
	r := Reboot{Node: node, State: RebootPending, Since: now, Started: now, Updated: now}
	if prev.Node == node && prev.State == RebootFailed {
		r.Cordoned = prev.Cordoned
	}
	return r
}

// Drain is how far a node's drain has come.
type Drain struct {
	Moving []string          // replicas neither running elsewhere yet nor held
	Held   map[string]string // replica -> why it stays
}

// CanBegin says why a pending reboot must wait, or "" when it may begin: one node at a time,
// on a healthy cluster.
func CanBegin(r Reboot, all map[string]Reboot, h Health) string {
	for _, n := range slices.Sorted(maps.Keys(all)) {
		if o := all[n]; n != r.Node && o.Active() {
			return fmt.Sprintf("waiting for %s's reboot", n)
		}
	}
	if !h.OK {
		return "waiting for the cluster: " + h.Why
	}
	return ""
}

// StepReboot advances a begun reboot by what the node reports and reports whether it changed.
func StepReboot(r Reboot, rep Report, d Drain, now time.Time) (Reboot, bool) {
	before, held := r.State, r.Held
	switch r.State {
	case RebootDraining:
		r.Held = d.Held
		switch {
		case len(d.Moving) == 0:
			r.State, r.Since = RebootRebooting, now
		case now.Sub(r.Since) > DrainTimeout:
			r.State = RebootFailed
			r.Reason = fmt.Sprintf("%s: not drained after %s; still moving: %s", r.Node, DrainTimeout, strings.Join(d.Moving, ", "))
		}
	case RebootRebooting:
		switch {
		case rep.BootID != "" && rep.BootID != r.BootID && rep.RebootNeeded:
			r.State = RebootFailed
			r.Reason = r.Node + ": rebooted, but still waits for a reboot: its boot default is not the system it runs"
		case rep.BootID != "" && rep.BootID != r.BootID:
			r.State = RebootDone
		case now.Sub(r.Since) > BackTimeout:
			r.State = RebootFailed
			r.Reason = fmt.Sprintf("%s: not back from its reboot after %s", r.Node, BackTimeout)
		}
	}
	if r.State == before && maps.Equal(r.Held, held) {
		return r, false
	}
	r.Updated = now
	return r, true
}

// ShouldReboot reports whether node must reboot itself now: its reboot says so, and it is
// still in the boot the reboot began in.
func ShouldReboot(r Reboot, node, bootID string) bool {
	return r.Node == node && r.State == RebootRebooting && bootID != "" && bootID == r.BootID
}

// NeedsRebootJob reports whether an automated rollout must start its current node's reboot.
func NeedsRebootJob(p Plan, reports map[string]Report, jobs map[string]Reboot) bool {
	if p.State != Running || !p.Reboot || p.Current == "" {
		return false
	}
	r := reports[p.Current]
	if r.Version != p.Target || !r.RebootNeeded {
		return false
	}
	j, ok := jobs[p.Current]
	return !ok || (!j.Open() && j.Started.Before(p.Started))
}
