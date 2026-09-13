// Package nodelc implements the node lifecycle (§4.8): cordon /
// uncordon / drain / remove with their safety interlocks, the
// /cluster/revoked/<id> registry whose entries permanently reject a
// removed node's identity, and the leader-side failure monitor that
// marks silent nodes unreachable (15 s) and failed (5 min).
//
// Lifecycle state lives on the node record (/nodes/<id>, join.NodeRecord)
// so every CLI and the status report see one source of truth:
//
//	Cordoned bool  — no new placements
//	State    string — "" (healthy) | unreachable | failed
//
// Removal is a human decision end-to-end: nothing here ever calls
// raft.RemoveServer automatically (§4.8 — auto-removal would eject
// nodes during a network blip).
package nodelc

import (
	"context"
	"encoding/json"
	"time"

	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
)

// Lifecycle states stored on the node record. The empty string means
// healthy — records written by the join flow predate the field.
const (
	StateUnreachable = "unreachable" // silent past UnreachableAfter
	StateFailed      = "failed"      // silent past FailedAfter; placements evicted
)

// Failure-detection defaults (§4.8). Overridable for tests.
const (
	DefaultUnreachableAfter = 15 * time.Second
	DefaultFailedAfter      = 5 * time.Minute
	DefaultInterval         = 5 * time.Second
)

// RevokedKeyPrefix holds removed node identities: /cluster/revoked/<id>.
// A record here permanently rejects the node's identity — re-joining
// with the same node ID is refused (§4.8).
const RevokedKeyPrefix = "/cluster/revoked/"

// Revocation is the /cluster/revoked/<id> value.
type Revocation struct {
	NodeID    string `json:"id"`
	RemovedAt int64  `json:"removed_at"` // unix-nano
	By        string `json:"by"`         // operator node that performed the removal
	Reason    string `json:"reason,omitempty"`
}

// Options tunes the lifecycle operations (all optional).
type Options struct {
	// By is the acting operator/node ID recorded in revocations.
	By string
	// Reason is recorded in the revocation.
	Reason string
	// Force permits quorum-breaking removals; Confirm must equal the
	// node ID when Force is set (the CLI's typed confirmation).
	Force   bool
	Confirm string
	// IgnoreUnplaceable lets drain proceed even when some of the
	// node's resources have nowhere else to run.
	IgnoreUnplaceable bool
	// Now overrides the clock (tests). Defaults to time.Now.
	Now func() time.Time
}

// Removal breaks quorum when the cluster has ≤2 voters: after removal
// only one voter remains and the next write needs the old quorum
// anyway (§4.8's "removing 1 of 2 voters" case).
func breaksQuorum(voters int) bool { return voters <= 2 }

func nowOr(o *Options) time.Time {
	if o != nil && o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// loadRecord fetches and decodes the node record, KindNotFound if absent.
func loadRecord(ctx context.Context, st *raftstore.Store, id string) (*join.NodeRecord, *store.Entry, error) {
	e, err := st.Get(ctx, store.Key(join.NodesKeyPrefix+id))
	if err != nil {
		return nil, nil, errors.Wrap(err, errors.KindNotFound, "nodelc", "node "+id+" not found")
	}
	var r join.NodeRecord
	if err := json.Unmarshal(e.Value, &r); err != nil {
		return nil, nil, errors.New(errors.KindInternal, "nodelc", "corrupt node record for "+id+": "+err.Error())
	}
	return &r, e, nil
}

// saveRecord CAS-writes the record at its current revision.
func saveRecord(ctx context.Context, st *raftstore.Store, r *join.NodeRecord, rev store.Revision) error {
	b, err := json.Marshal(r)
	if err != nil {
		return errors.New(errors.KindInternal, "nodelc", "encode record: "+err.Error())
	}
	if _, err := st.CompareAndSwap(ctx, store.Key(join.NodesKeyPrefix+r.ID), rev, b); err != nil {
		return errors.Wrap(err, errors.KindConflict, "nodelc", "node record changed concurrently")
	}
	return nil
}

// voters lists node records participating in raft quorum (role "" or
// "voter" — witnesses are full voters per §4.9).
func voters(ctx context.Context, st *raftstore.Store) ([]join.NodeRecord, error) {
	entries, err := st.List(ctx, store.Key(join.NodesKeyPrefix))
	if err != nil {
		return nil, errors.Wrap(err, errors.KindUnavailable, "nodelc", "list nodes: "+err.Error())
	}
	var out []join.NodeRecord
	for _, e := range entries {
		var r join.NodeRecord
		if json.Unmarshal(e.Value, &r) != nil || r.ID == "" {
			continue
		}
		if r.Role == "" || r.Role == "voter" {
			out = append(out, r)
		}
	}
	return out, nil
}

// placeable reports whether another node can host new work: a voter
// that is up (not unreachable/failed) and not cordoned.
func placeable(r join.NodeRecord) bool {
	switch r.State {
	case StateUnreachable, StateFailed:
		return false
	}
	return !r.Cordoned
}

// Cordon marks the node as unavailable for new placements (§4.8).
func Cordon(ctx context.Context, st *raftstore.Store, id string) error {
	r, e, err := loadRecord(ctx, st, id)
	if err != nil {
		return err
	}
	if r.Cordoned {
		return nil // idempotent
	}
	r.Cordoned = true
	return saveRecord(ctx, st, r, e.Revision)
}

// Uncordon clears the cordon.
func Uncordon(ctx context.Context, st *raftstore.Store, id string) error {
	r, e, err := loadRecord(ctx, st, id)
	if err != nil {
		return err
	}
	if !r.Cordoned {
		return nil
	}
	r.Cordoned = false
	return saveRecord(ctx, st, r, e.Revision)
}

// Drain cordons the node and verifies every desired resource on it
// could be re-placed elsewhere (§4.8). The actual move happens when
// the placement engine lands; drain is the gate that guarantees it
// will be possible. Returns the number of resources that must move.
func Drain(ctx context.Context, st *raftstore.Store, id string, opts *Options) (int, error) {
	if _, _, err := loadRecord(ctx, st, id); err != nil {
		return 0, err
	}
	if err := Cordon(ctx, st, id); err != nil {
		return 0, err
	}

	// Resources on the node (the reconciler's per-node desired state).
	res, err := st.List(ctx, store.Key("/node/"+id+"/resources/"))
	if err != nil {
		return 0, errors.Wrap(err, errors.KindUnavailable, "nodelc", "list node resources: "+err.Error())
	}
	if len(res) == 0 {
		return 0, nil
	}

	// Another placeable node?
	others, err := voters(ctx, st)
	if err != nil {
		return 0, err
	}
	for i := range others {
		if others[i].ID != id && placeable(others[i]) {
			return len(res), nil
		}
	}

	ignore := opts != nil && opts.IgnoreUnplaceable
	if !ignore {
		return len(res), errors.New(errors.KindConflict, "nodelc",
			"drain: no other placeable node can host the resources; use --ignore-unplaceable to proceed anyway")
	}
	return len(res), nil
}

// Remove drains-and-purges a node from the cluster (§4.8):
//
//	refuse if it would break quorum (≤2 voters) unless Force + typed Confirm
//	→ raft.RemoveServer
//	→ one raft txn: write /cluster/revoked/<id> + delete /nodes/<id>
//
// The revocation is what rejects the node's identity hereafter: the
// join service refuses any re-join under a revoked node ID (§4.8).
// Removing the leader is refused — transfer leadership first.
func Remove(ctx context.Context, st *raftstore.Store, id string, opts *Options) error {
	if opts == nil {
		opts = &Options{}
	}
	if !st.IsLeader() {
		return errors.New(errors.KindUnavailable, "nodelc",
			"remove: this node is not the leader — run `expanse ctl node remove` on the leader")
	}
	if id == st.NodeID() {
		return errors.New(errors.KindConflict, "nodelc",
			"remove: refusing to remove the local (leader) node; transfer leadership first")
	}

	_, entry, err := loadRecord(ctx, st, id)
	if err != nil {
		return err
	}

	vs, err := voters(ctx, st)
	if err != nil {
		return err
	}
	if breaksQuorum(len(vs)) {
		if !opts.Force {
			return errors.New(errors.KindConflict, "nodelc",
				"remove: removing this node would break quorum — retry with --force and type the node name to confirm")
		}
		if opts.Confirm != id {
			return errors.New(errors.KindPermission, "nodelc",
				"remove: --force requires typing the node name to confirm")
		}
	}

	if err := st.RemoveServer(id); err != nil {
		return errors.Wrap(err, errors.KindUnavailable, "nodelc", "RemoveServer: "+err.Error())
	}

	rev := Revocation{NodeID: id, RemovedAt: nowOr(opts).UnixNano(), By: opts.By, Reason: opts.Reason}
	rb, err := json.Marshal(rev)
	if err != nil {
		return errors.New(errors.KindInternal, "nodelc", "encode revocation: "+err.Error())
	}
	ops := []store.Op{{Kind: store.OpPut, Key: store.Key(RevokedKeyPrefix + id), Value: rb}}
	if entry != nil {
		// Delete the node record only if it hasn't changed since we
		// read it; a concurrent join re-writing the record fails the
		// whole txn, leaving a consistent (still-enrolled) state.
		ops = append(ops, store.Op{Kind: store.OpDelete, Key: store.Key(join.NodesKeyPrefix + id), Expect: entry.Revision})
	}
	if _, err := st.Txn(ctx, ops); err != nil {
		return errors.Wrap(err, errors.KindInternal, "nodelc", "revocation txn: "+err.Error())
	}
	return nil
}

// IsRevoked reports whether the node ID appears in /cluster/revoked/.
func IsRevoked(ctx context.Context, st store.Store, id string) (bool, error) {
	_, err := st.Get(ctx, store.Key(RevokedKeyPrefix+id))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, errors.KindNotFound):
		return false, nil
	default:
		return false, err
	}
}

// Transition is one lifecycle state change made by the Monitor.
type Transition struct {
	NodeID string
	From   string // "" = healthy
	To     string
}

// Monitor is the leader-side failure detector (§4.8): a node whose
// status entry has not updated for UnreachableAfter is marked
// unreachable; past FailedAfter it is marked failed and its placements
// evicted (eviction currently a no-op hook — the placement engine has
// not landed). Nodes are never auto-removed from raft.
type Monitor struct {
	St               *raftstore.Store
	ThisNodeID       string // local node; only the leader evaluates
	UnreachableAfter time.Duration
	FailedAfter      time.Duration
	Interval         time.Duration
	// Evict, when set, is called after a node transitions to failed.
	// It must be idempotent.
	Evict func(nodeID string)
	// Now is the clock (fake in tests). Defaults to time.Now.
	Now func() time.Time
}

// Run drives the monitor until ctx is canceled. Only the leader
// evaluates; followers idle (state converges when the leader's writes
// replicate).
func (m *Monitor) Run(ctx context.Context) {
	ival := m.Interval
	if ival <= 0 {
		ival = DefaultInterval
	}
	t := time.NewTicker(ival)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Evaluate(ctx, time.Now())
		}
	}
}

// Evaluate performs one detection pass at the given instant and
// returns the transitions it made. Deterministic given the store state
// and `now` — tests drive it with a fake clock.
func (m *Monitor) Evaluate(ctx context.Context, now time.Time) []Transition {
	if !m.St.IsLeader() {
		return nil
	}
	unr := m.UnreachableAfter
	if unr <= 0 {
		unr = DefaultUnreachableAfter
	}
	fail := m.FailedAfter
	if fail <= 0 {
		fail = DefaultFailedAfter
	}

	entries, err := m.St.List(ctx, store.Key(join.NodesKeyPrefix))
	if err != nil {
		return nil
	}
	var out []Transition
	for _, e := range entries {
		var r join.NodeRecord
		if json.Unmarshal(e.Value, &r) != nil || r.ID == "" {
			continue
		}
		if r.State == StateFailed {
			continue
		}
		// Last seen: the status entry's store timestamp (FSM-assigned,
		// from the leader's command clock), falling back to join time.
		last := time.Unix(0, r.JoinedAt)
		if se, err := m.St.Get(ctx, store.Key(join.NodesKeyPrefix+r.ID+"/status")); err == nil {
			last = time.Unix(0, se.UpdatedAt)
		}
		silent := now.Sub(last)

		want := ""
		switch {
		case silent >= fail:
			want = StateFailed
		case silent >= unr:
			want = StateUnreachable
		}
		if want == "" || want == r.State {
			continue
		}
		from := r.State
		r.State = want
		if err := saveRecord(ctx, m.St, &r, e.Revision); err != nil {
			continue // raced; next pass re-evaluates
		}
		out = append(out, Transition{NodeID: r.ID, From: from, To: want})
		if want == StateFailed && m.Evict != nil {
			m.Evict(r.ID)
		}
	}
	return out
}
