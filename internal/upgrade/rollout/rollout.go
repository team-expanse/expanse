// Package rollout walks a cluster to a release one node at a time. The leader advances the
// plan (Step); each node starts its own upgrade when the plan names it (ShouldStart).
package rollout

import (
	"fmt"
	"slices"
	"time"

	"github.com/expanse/expanse/internal/upgrade"
)

// State is the plan's.
type State string

const (
	Running State = "running"
	Paused  State = "paused"
	Halted  State = "halted"
	Done    State = "done"
)

// NodeTimeout halts a rollout whose current node has reported nothing for this long.
const NodeTimeout = 45 * time.Minute

// Plan is the cluster's rollout, stored at PlanKey.
type Plan struct {
	Target  string   `json:"target"`
	State   State    `json:"state"`
	Order   []string `json:"order"`
	Current string   `json:"current,omitempty"`
	// CurrentSince is when Current was picked; NodeTimeout counts from it.
	CurrentSince time.Time `json:"currentSince,omitempty"`
	// Reason says why the plan waits or halted.
	Reason string `json:"reason,omitempty"`
	// Started is set on start and on every resume; node upgrades older than it are retried.
	Started time.Time `json:"started"`
	Updated time.Time `json:"updated"`
}

// Report is what a node says about itself, stored under NodeKey.
type Report struct {
	Version string          `json:"version"`
	Upgrade *upgrade.Status `json:"upgrade,omitempty"`
	Updated time.Time       `json:"updated"`
}

// Health gates each node's start and the move to the next one.
type Health struct {
	OK  bool
	Why string
}

// Order upgrades the leader last, so the rollout causes one election, not one per node.
func Order(nodes []string, leader string) []string {
	out := slices.Clone(nodes)
	slices.Sort(out)
	out = slices.DeleteFunc(out, func(n string) bool { return n == leader })
	if slices.Contains(nodes, leader) {
		out = append(out, leader)
	}
	return out
}

// Step advances a running plan by at most one move and reports whether it changed.
func Step(p Plan, reports map[string]Report, h Health, now time.Time) (Plan, bool) {
	if p.State != Running {
		return p, false
	}
	before := p
	if p.Current != "" {
		p = stepCurrent(p, reports[p.Current], h, now)
	}
	if p.Current == "" && p.State == Running {
		p = pickNext(p, reports, h, now)
	}
	if samePlan(before, p) {
		return before, false
	}
	p.Updated = now
	return p, true
}

func stepCurrent(p Plan, r Report, h Health, now time.Time) Plan {
	if u := r.Upgrade; u != nil && u.Version == p.Target && u.State == upgrade.StateFailed && !u.Started.Before(p.Started) {
		p.State, p.Reason = Halted, fmt.Sprintf("%s: %s", p.Current, u.Error)
		return p
	}
	if r.Version == p.Target {
		if !h.OK {
			p.Reason = "waiting for the cluster: " + h.Why
			return p
		}
		p.Current, p.Reason = "", ""
		return p
	}
	if seen := lastSeen(r); now.Sub(seen) > NodeTimeout && now.Sub(p.CurrentSince) > NodeTimeout {
		p.State, p.Reason = Halted, fmt.Sprintf("%s: no report for %s", p.Current, NodeTimeout)
	}
	return p
}

func pickNext(p Plan, reports map[string]Report, h Health, now time.Time) Plan {
	for _, n := range p.Order {
		if reports[n].Version == p.Target {
			continue
		}
		if !h.OK {
			p.Reason = "waiting for the cluster: " + h.Why
			return p
		}
		p.Current, p.CurrentSince, p.Reason = n, now, ""
		return p
	}
	p.State, p.Reason = Done, ""
	return p
}

func lastSeen(r Report) time.Time {
	if r.Upgrade != nil && r.Upgrade.Updated.After(r.Updated) {
		return r.Upgrade.Updated
	}
	return r.Updated
}

func samePlan(a, b Plan) bool {
	return a.State == b.State && a.Current == b.Current && a.Reason == b.Reason
}

// ShouldStart reports whether node must begin its upgrade now: it is current, not on the
// target, and has no attempt at the target since the plan (re)started.
func ShouldStart(p Plan, node string, r Report) bool {
	if p.State != Running || p.Current != node || r.Version == p.Target {
		return false
	}
	u := r.Upgrade
	return u == nil || u.Version != p.Target || u.Started.Before(p.Started)
}
