package chaos

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/expanse/expanse/internal/cluster/lease"
	"github.com/expanse/expanse/internal/store"
)

// Violation is one observed invariant breach. A single violation is a
// release blocker (§6).
type Violation struct {
	Kind    string // "split-brain" | "revision-regression"
	Detail  string
	At      time.Time
	Holders []string // split-brain: who allegedly holds the lease
}

// NodeProbe lets the checker inspect one node's LOCAL FSM view (stale
// reads — deliberately not linearizable: a partitioned or deposed node
// must still be observable).
type NodeProbe struct {
	ID string
	// ListLeases returns every lease record as this node sees it.
	ListLeases func(ctx context.Context) ([]*lease.Lease, error)
}

// HarnessLeaseProbe returns a probe over node i's local FSM view,
// re-resolving the store on every poll so Kill/Restart cycles stay
// observable.
func (h *Harness) HarnessLeaseProbe(i int) NodeProbe {
	return NodeProbe{
		ID: h.nodeID(i),
		ListLeases: func(ctx context.Context) ([]*lease.Lease, error) {
			s := h.Node(i)
			if s == nil || !h.Live(i) {
				return nil, fmt.Errorf("n%d down", i)
			}
			entries, err := s.List(store.WithStale(ctx), store.Key(lease.Prefix))
			if err != nil {
				return nil, err
			}
			out := make([]*lease.Lease, 0, len(entries))
			for _, e := range entries {
				name := string(e.Key)[len(lease.Prefix):]
				l, ierr := lease.Inspect(store.WithStale(ctx), s, name)
				if ierr != nil {
					return nil, ierr
				}
				if l != nil {
					out = append(out, l)
				}
			}
			return out, nil
		},
	}
}

// Checker polls every node every Interval and asserts the §6 invariants:
//
//  1. Split brain: for each lease, take the maximum record revision seen
//     anywhere; a node counts as a holder only if it carries the
//     max-revision record AND that record is unexpired. More than one
//     distinct such holder = two FSMs disagreeing on the newest committed
//     state = raft broken. (A stale lower-revision copy on a partitioned
//     node is NOT a holder: the majority legitimately takes over an
//     expired lease while the minority still shows the old record, which
//     is exactly the guard-band takeover path.)
//  2. Revision regression: no node may ever observe a lease record whose
//     revision is lower than one it observed earlier at the same key —
//     log truncation / rollback alarm.
//
// Any violation is recorded; a single violation is a release blocker.
type Checker struct {
	Interval time.Duration
	Probes   []NodeProbe
	Now      func() time.Time // nil = time.Now

	mu         sync.Mutex
	violations []Violation
	observed   map[string]map[string]store.Revision // node → lease → last rev
}

func NewChecker(interval time.Duration, probes ...NodeProbe) *Checker {
	return &Checker{
		Interval: interval,
		Probes:   probes,
		observed: make(map[string]map[string]store.Revision),
	}
}

// Run polls until ctx is done. Safe to run in a goroutine.
func (c *Checker) Run(ctx context.Context) {
	now := c.Now
	if now == nil {
		now = time.Now
	}
	t := time.NewTicker(c.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		tickCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.Interval)
		c.tick(tickCtx, now())
		cancel()
	}
}

type leaseView struct {
	holder  string
	rev     store.Revision
	expires time.Time
}

func (c *Checker) tick(ctx context.Context, now time.Time) {
	perLease := make(map[string][]leaseView)
	for _, p := range c.Probes {
		leases, err := p.ListLeases(ctx)
		if err != nil {
			continue // node down / partitioned: skip this tick
		}
		c.mu.Lock()
		if c.observed[p.ID] == nil {
			c.observed[p.ID] = make(map[string]store.Revision)
		}
		for _, l := range leases {
			prev, ok := c.observed[p.ID][l.Name]
			if ok && l.Revision < prev {
				c.addViolation(Violation{
					Kind:   "revision-regression",
					Detail: fmt.Sprintf("n%s observed %s revision %d after %d", p.ID, l.Name, l.Revision, prev),
					At:     now,
				})
			}
			if l.Revision > prev {
				c.observed[p.ID][l.Name] = l.Revision
			}
		}
		c.mu.Unlock()
		for _, l := range leases {
			perLease[l.Name] = append(perLease[l.Name], leaseView{holder: l.Holder, rev: l.Revision, expires: l.ExpiresAt})
		}
	}
	for name, views := range perLease {
		maxRev := store.Revision(0)
		for _, v := range views {
			if v.rev > maxRev {
				maxRev = v.rev
			}
		}
		// Distinct holders among nodes carrying the newest record,
		// unexpired.
		uniq := []string{}
		seenH := make(map[string]bool)
		for _, v := range views {
			if v.rev == maxRev && v.expires.After(now) && !seenH[v.holder] {
				seenH[v.holder] = true
				uniq = append(uniq, v.holder)
			}
		}
		if len(uniq) > 1 {
			c.addViolation(Violation{
				Kind:    "split-brain",
				Detail:  fmt.Sprintf("lease %s at revision %d held by %v", name, maxRev, uniq),
				At:      now,
				Holders: uniq,
			})
		}
	}
}

func (c *Checker) addViolation(v Violation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.violations = append(c.violations, v)
}

// Violations returns all violations observed so far.
func (c *Checker) Violations() []Violation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Violation(nil), c.violations...)
}

// Clean reports whether no violation has been observed.
func (c *Checker) Clean() bool { return len(c.Violations()) == 0 }
