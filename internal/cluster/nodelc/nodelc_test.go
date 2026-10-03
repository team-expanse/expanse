package nodelc_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/cluster/nodelc"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
)

// newLCEnv boots a one-node raftstore (the bolt lock makes a full
// multi-node rig unnecessary: the lifecycle logic operates on node
// RECORDS, which the tests write directly; raft interplay —
// RemoveServer, txn commit — runs against the real leader).
func newLCEnv(t *testing.T) (*raftstore.Store, context.Context) {
	t.Helper()
	st, err := raftstore.Open(raftstore.Config{
		NodeID: "n0", BindAddr: "127.0.0.1:0", DataDir: t.TempDir(), Bootstrap: true,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	deadline := time.Now().Add(10 * time.Second)
	for !st.IsLeader() {
		if time.Now().After(deadline) {
			t.Fatal("no leader within timeout")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return st, context.Background()
}

// writeNode hand-writes a /nodes/<id> record (join.NodeRecord) — the
// state the join flow leaves behind, here synthesized per scenario.
func writeNode(t *testing.T, ctx context.Context, st *raftstore.Store, id, role string) {
	t.Helper()
	r := join.NodeRecord{ID: id, Role: role, RaftAddr: "127.0.0.1:7444", JoinedAt: time.Now().UnixNano()}
	if role == "" {
		r.Role = "voter"
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	if _, err := st.Put(ctx, store.Key(join.NodesKeyPrefix+id), b); err != nil {
		t.Fatalf("write record %s: %v", id, err)
	}
}

func record(t *testing.T, ctx context.Context, st *raftstore.Store, id string) join.NodeRecord {
	t.Helper()
	e, err := st.Get(ctx, store.Key(join.NodesKeyPrefix+id))
	if err != nil {
		t.Fatalf("record %s: %v", id, err)
	}
	var r join.NodeRecord
	if err := json.Unmarshal(e.Value, &r); err != nil {
		t.Fatalf("unmarshal record %s: %v", id, err)
	}
	return r
}

func writeStatus(t *testing.T, ctx context.Context, st *raftstore.Store, id string) {
	t.Helper()
	if _, err := st.Put(ctx, store.Key(join.NodesKeyPrefix+id+"/status"), []byte("health=healthy")); err != nil {
		t.Fatalf("write status: %v", err)
	}
}

// TestRemoveInterlockMatrix covers §4.8's removal guardrails.
func TestRemoveInterlockMatrix(t *testing.T) {
	type scenario struct {
		name    string
		voters  int    // synthetic voter records besides n0
		target  string // which node to remove
		force   bool
		confirm string
		wantErr bool
		wantKnd errors.Kind
	}
	// `voters` counts TOTAL voter records, including the local leader
	// n0 — the harness writes n0..n(voters-1).
	cases := []scenario{
		{name: "missing node", voters: 3, target: "ghost", wantErr: true, wantKnd: errors.KindNotFound},
		{name: "1 of 2 voters refused", voters: 2, target: "n1", wantErr: true, wantKnd: errors.KindConflict},
		{name: "1 of 2 voters force needs confirm", voters: 2, target: "n1", force: true, wantErr: true, wantKnd: errors.KindPermission},
		{name: "1 of 2 voters wrong confirm", voters: 2, target: "n1", force: true, confirm: "n2", wantErr: true, wantKnd: errors.KindPermission},
		{name: "1 of 2 voters force + typed name", voters: 2, target: "n1", force: true, confirm: "n1"},
		{name: "1 of 3 voters fine", voters: 3, target: "n1"},
		{name: "remove leader refused", voters: 3, target: "n0", wantErr: true, wantKnd: errors.KindConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, ctx := newLCEnv(t)
			for i := 0; i < tc.voters; i++ {
				writeNode(t, ctx, st, fmt.Sprintf("n%d", i), "")
			}
			opts := &nodelc.Options{Force: tc.force, Confirm: tc.confirm, By: "op"}
			err := nodelc.Remove(ctx, st, tc.target, opts)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil")
				}
				if !errors.Is(err, tc.wantKnd) {
					t.Fatalf("err = %v, want kind %v", err, tc.wantKnd)
				}
				return
			}
			if err != nil {
				t.Fatalf("Remove: %v", err)
			}
			// Record deleted, revocation written.
			if _, err := st.Get(ctx, store.Key(join.NodesKeyPrefix+tc.target)); !errors.Is(err, errors.KindNotFound) {
				t.Errorf("node record still present: %v", err)
			}
			rev, err := nodelc.IsRevoked(ctx, st, tc.target)
			if err != nil || !rev {
				t.Errorf("IsRevoked = %v, %v; want true", rev, err)
			}
		})
	}
}

// TestRemoveLeaderNamesTheTransferCommand: the refusal points at a
// command that exists.
func TestRemoveLeaderNamesTheTransferCommand(t *testing.T) {
	st, ctx := newLCEnv(t)
	for _, id := range []string{"n0", "n1", "n2"} {
		writeNode(t, ctx, st, id, "")
	}
	err := nodelc.Remove(ctx, st, "n0", nil)
	if err == nil || !strings.Contains(err.Error(), "expanse ctl node transfer-leadership") {
		t.Errorf("Remove(leader) = %v, want a pointer to `expanse ctl node transfer-leadership`", err)
	}
}

// TestRemoveRevocationRecordContent checks the revocation payload.
func TestRemoveRevocationRecordContent(t *testing.T) {
	st, ctx := newLCEnv(t)
	writeNode(t, ctx, st, "n0", "")
	writeNode(t, ctx, st, "n1", "")
	writeNode(t, ctx, st, "n2", "")
	if err := nodelc.Remove(ctx, st, "n1", &nodelc.Options{By: "op", Reason: "decommission"}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	e, err := st.Get(ctx, store.Key(join.RevokedKeyPrefix+"n1"))
	if err != nil {
		t.Fatalf("revocation record: %v", err)
	}
	var r nodelc.Revocation
	if err := json.Unmarshal(e.Value, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.NodeID != "n1" || r.By != "op" || r.Reason != "decommission" || r.RemovedAt == 0 {
		t.Errorf("revocation = %+v", r)
	}
}

// TestCordonUncordon: idempotent flag flips on the record.
func TestCordonUncordon(t *testing.T) {
	st, ctx := newLCEnv(t)
	writeNode(t, ctx, st, "n1", "")

	if err := nodelc.Cordon(ctx, st, "n1"); err != nil {
		t.Fatalf("Cordon: %v", err)
	}
	if err := nodelc.Cordon(ctx, st, "n1"); err != nil {
		t.Fatalf("Cordon (again): %v", err)
	}
	if !record(t, ctx, st, "n1").Cordoned {
		t.Error("record not cordoned")
	}
	if err := nodelc.Uncordon(ctx, st, "n1"); err != nil {
		t.Fatalf("Uncordon: %v", err)
	}
	if err := nodelc.Uncordon(ctx, st, "n1"); err != nil {
		t.Fatalf("Uncordon (again): %v", err)
	}
	if record(t, ctx, st, "n1").Cordoned {
		t.Error("record still cordoned")
	}
	if err := nodelc.Cordon(ctx, st, "ghost"); !errors.Is(err, errors.KindNotFound) {
		t.Errorf("Cordon(ghost) = %v, want NotFound", err)
	}
}

// Drain marks the node draining so its replicas move; uncordon ends it.
// A refused drain leaves the node undrained.
func TestDrainMarksDrainingUntilUncordon(t *testing.T) {
	st, ctx := newLCEnv(t)
	writeNode(t, ctx, st, "n0", "")
	writeNode(t, ctx, st, "n1", "")
	writeRes(t, ctx, st, "n1", "web")
	if err := nodelc.Cordon(ctx, st, "n0"); err != nil {
		t.Fatalf("cordon n0: %v", err)
	}
	if _, err := nodelc.Drain(ctx, st, "n1", nil); !errors.Is(err, errors.KindConflict) {
		t.Fatalf("Drain err = %v, want Conflict", err)
	}
	if r := record(t, ctx, st, "n1"); r.Draining {
		t.Error("refused drain marked the node draining")
	}
	if _, err := nodelc.Drain(ctx, st, "n1", &nodelc.Options{IgnoreUnplaceable: true}); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if r := record(t, ctx, st, "n1"); !r.Cordoned || !r.Draining {
		t.Errorf("after drain cordoned=%t draining=%t, want both", r.Cordoned, r.Draining)
	}
	infos, err := nodelc.List(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range infos {
		if in.ID == "n1" && !in.Draining {
			t.Error("List does not report n1 draining")
		}
	}
	if err := nodelc.Uncordon(ctx, st, "n1"); err != nil {
		t.Fatalf("Uncordon: %v", err)
	}
	if r := record(t, ctx, st, "n1"); r.Cordoned || r.Draining {
		t.Errorf("after uncordon cordoned=%t draining=%t, want neither", r.Cordoned, r.Draining)
	}
}

// writeRes writes a desired resource for a node (what ApplySpec leaves
// behind).
func writeRes(t *testing.T, ctx context.Context, st *raftstore.Store, node, id string) {
	t.Helper()
	if _, err := st.Put(ctx, store.Key("/node/"+node+"/resources/"+id), []byte("type: file")); err != nil {
		t.Fatalf("put resource: %v", err)
	}
}

// TestDrainInterlock: drain cordons and verifies re-placability.
func TestDrainInterlock(t *testing.T) {
	t.Run("no resources", func(t *testing.T) {
		st, ctx := newLCEnv(t)
		writeNode(t, ctx, st, "n1", "")
		n, err := nodelc.Drain(ctx, st, "n1", nil)
		if err != nil || n != 0 {
			t.Fatalf("Drain = %d, %v; want 0, nil", n, err)
		}
		if !record(t, ctx, st, "n1").Cordoned {
			t.Error("drain did not cordon")
		}
	})

	t.Run("unplaceable refused", func(t *testing.T) {
		st, ctx := newLCEnv(t)
		writeNode(t, ctx, st, "n1", "") // only other voter is... none besides n0? n0 IS placeable
		// n0 is the local leader — it counts as placeable, so make it
		// cordoned via its record to force the refusal path.
		writeNode(t, ctx, st, "n0", "")
		writeRes(t, ctx, st, "n1", "web")
		if err := nodelc.Cordon(ctx, st, "n0"); err != nil {
			t.Fatalf("cordon n0: %v", err)
		}
		if _, err := nodelc.Drain(ctx, st, "n1", nil); !errors.Is(err, errors.KindConflict) {
			t.Fatalf("Drain err = %v, want Conflict", err)
		}
		n, err := nodelc.Drain(ctx, st, "n1", &nodelc.Options{IgnoreUnplaceable: true})
		if err != nil || n != 1 {
			t.Fatalf("Drain(ignore) = %d, %v; want 1, nil", n, err)
		}
	})

	t.Run("placeable elsewhere", func(t *testing.T) {
		st, ctx := newLCEnv(t)
		writeNode(t, ctx, st, "n1", "")
		writeNode(t, ctx, st, "n2", "")
		writeRes(t, ctx, st, "n1", "web")
		writeRes(t, ctx, st, "n1", "db")
		n, err := nodelc.Drain(ctx, st, "n1", nil)
		if err != nil || n != 2 {
			t.Fatalf("Drain = %d, %v; want 2, nil", n, err)
		}
	})

	t.Run("unhealthy neighbor not placeable", func(t *testing.T) {
		st, ctx := newLCEnv(t)
		writeNode(t, ctx, st, "n1", "")
		writeNode(t, ctx, st, "n0", "") // explicit leader record so it can be cordoned
		writeNode(t, ctx, st, "n2", "")
		writeRes(t, ctx, st, "n1", "web")
		e2, err := st.Get(ctx, store.Key(join.NodesKeyPrefix+"n2"))
		if err != nil {
			t.Fatalf("get n2: %v", err)
		}
		var rec join.NodeRecord
		if err := json.Unmarshal(e2.Value, &rec); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		rec.State = nodelc.StateFailed
		b, _ := json.Marshal(rec)
		if _, err := st.CompareAndSwap(ctx, store.Key(join.NodesKeyPrefix+"n2"), e2.Revision, b); err != nil {
			t.Fatalf("mark failed: %v", err)
		}
		if err := nodelc.Cordon(ctx, st, "n0"); err != nil {
			t.Fatalf("cordon n0: %v", err)
		}
		if _, err := nodelc.Drain(ctx, st, "n1", nil); !errors.Is(err, errors.KindConflict) {
			t.Fatalf("Drain err = %v, want Conflict (failed/cordoned peers not placeable)", err)
		}
	})
}

// TestMonitorStateTransitions drives the §4.8 failure machine with a
// fake clock: silent 15 s → unreachable; silent 5 min → failed + evict;
// nothing fires before the thresholds; already-failed nodes don't
// re-transition. `base` is the real write time of n1's status; fake
// nows are offsets from it (status Entry.UpdatedAt is FSM-assigned real
// time, so silence is measured from the last real write).
func TestMonitorStateTransitions(t *testing.T) {
	st, ctx := newLCEnv(t)
	writeNode(t, ctx, st, "n1", "")
	writeStatus(t, ctx, st, "n1")
	base := time.Now()

	evicted := []string{}
	m := &nodelc.Monitor{
		St:         st,
		ThisNodeID: "n0",
		Evict:      func(id string) { evicted = append(evicted, id) },
	}

	// t+10s: inside the 15 s window — nothing.
	if ts := m.Evaluate(ctx, base.Add(10*time.Second)); len(ts) != 0 {
		t.Fatalf("t+10s transitions = %+v, want none", ts)
	}
	// t+16s: n1 unreachable.
	ts := m.Evaluate(ctx, base.Add(16*time.Second))
	if len(ts) != 1 || ts[0].NodeID != "n1" || ts[0].To != nodelc.StateUnreachable {
		t.Fatalf("t+16s transitions = %+v", ts)
	}
	if got := record(t, ctx, st, "n1").State; got != nodelc.StateUnreachable {
		t.Errorf("n1 state = %q, want unreachable", got)
	}
	// t+16s again: idempotent.
	if ts := m.Evaluate(ctx, base.Add(16*time.Second)); len(ts) != 0 {
		t.Fatalf("second t+16s transitions = %+v, want none", ts)
	}
	// t+2min: still just unreachable.
	if ts := m.Evaluate(ctx, base.Add(2*time.Minute)); len(ts) != 0 {
		t.Fatalf("t+2min transitions = %+v, want none", ts)
	}
	// t+6min: n1 failed + evicted.
	ts = m.Evaluate(ctx, base.Add(6*time.Minute))
	if len(ts) != 1 || ts[0].NodeID != "n1" || ts[0].To != nodelc.StateFailed {
		t.Fatalf("t+6min transitions = %+v", ts)
	}
	if len(evicted) != 1 {
		t.Errorf("evicted = %v, want [n1]", evicted)
	}
	if got := record(t, ctx, st, "n1").State; got != nodelc.StateFailed {
		t.Errorf("n1 state = %q, want failed", got)
	}
	// Past failed: no further transitions, no double eviction.
	if ts := m.Evaluate(ctx, base.Add(7*time.Minute)); len(ts) != 0 {
		t.Fatalf("t+7min transitions = %+v, want none", ts)
	}
}

// TestMonitorFreshNodeStaysHealthy: a node that renews its status
// inside the window is never marked; the silence clock restarts at
// each status write.
func TestMonitorFreshNodeStaysHealthy(t *testing.T) {
	st, ctx := newLCEnv(t)
	writeNode(t, ctx, st, "n1", "")
	m := &nodelc.Monitor{St: st, ThisNodeID: "n0"}

	for i := 0; i < 3; i++ {
		base := time.Now()
		writeStatus(t, ctx, st, "n1") // renewed now
		// 14 s later — inside the 15 s window.
		if ts := m.Evaluate(ctx, base.Add(14*time.Second)); len(ts) != 0 {
			t.Fatalf("round %d: transitions = %+v, want none", i, ts)
		}
	}
	if got := record(t, ctx, st, "n1").State; got != "" {
		t.Errorf("n1 state = %q, want healthy (empty)", got)
	}
}

// TestWitnessNeverPlaceable (§4.9): a witness votes (counts toward
// quorum) but is never a drain destination — zero advertised capacity.
func TestWitnessNeverPlaceable(t *testing.T) {
	st, ctx := newLCEnv(t)
	// n1 (the node being drained) and n2 = witness only — the leader's
	// record is deliberately absent so the witness is the sole neighbor.
	writeNode(t, ctx, st, "n1", "")
	// n2 as witness: role=witness, otherwise healthy.
	r := join.NodeRecord{ID: "n2", Role: "witness", RaftAddr: "127.0.0.1:1", JoinedAt: time.Now().UnixNano()}
	b, _ := json.Marshal(r)
	if _, err := st.Put(ctx, store.Key(join.NodesKeyPrefix+"n2"), b); err != nil {
		t.Fatalf("write witness record: %v", err)
	}
	writeRes(t, ctx, st, "n1", "web") // one resource on n1

	// Drain must refuse: the only "other" node is a witness.
	if _, err := nodelc.Drain(ctx, st, "n1", nil); !errors.Is(err, errors.KindConflict) {
		t.Fatalf("Drain with witness-only neighbor err = %v, want Conflict", err)
	}
	// Replacing the witness with a real voter unblocks the drain.
	r.Role = "voter"
	b2, _ := json.Marshal(r)
	e, err := st.Get(ctx, store.Key(join.NodesKeyPrefix+"n2"))
	if err != nil {
		t.Fatalf("get n2: %v", err)
	}
	if _, err := st.CompareAndSwap(ctx, store.Key(join.NodesKeyPrefix+"n2"), e.Revision, b2); err != nil {
		t.Fatalf("rewrite n2: %v", err)
	}
	n, err := nodelc.Drain(ctx, st, "n1", nil)
	if err != nil || n != 1 {
		t.Fatalf("Drain with real voter = %d, %v; want 1, nil", n, err)
	}
}

// TestMonitorRunLoop covers the tick loop: Run evaluates on every
// interval until the context is canceled.
func TestMonitorRunLoop(t *testing.T) {
	st, ctx := newLCEnv(t)
	writeNode(t, ctx, st, "n1", "")
	writeStatus(t, ctx, st, "n1")
	base := time.Now()

	evicted := []string{}
	m := &nodelc.Monitor{
		St:               st,
		ThisNodeID:       "n0",
		Evict:            func(id string) { evicted = append(evicted, id) },
		UnreachableAfter: 2 * time.Second,
		FailedAfter:      3 * time.Second,
		Interval:         40 * time.Millisecond,
	}
	runCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	m.Run(runCtx) // returns on ctx.Done
	_ = base
	_ = evicted
}

// §4.8 node restart: a failed/unreachable node that reports fresh again
// recovers — the detector state clears and the node is placeable.
func TestMonitorRecoveryClearsState(t *testing.T) {
	st, ctx := newLCEnv(t)
	writeNode(t, ctx, st, "n1", "")
	writeStatus(t, ctx, st, "n1")
	base := time.Now()
	m := &nodelc.Monitor{St: st, ThisNodeID: "n0"}

	if ts := m.Evaluate(ctx, base.Add(6*time.Minute)); len(ts) == 0 {
		t.Fatal("expected a transition at t+6min")
	}
	if got := record(t, ctx, st, "n1").State; got != nodelc.StateFailed {
		t.Fatalf("state = %q, want failed", got)
	}
	// The node restarts and its health loop refreshes the status entry.
	// The store stamps entries with the REAL clock; evaluate just past
	// it (silent ≈ 0 → want cleared).
	writeStatus(t, ctx, st, "n1")
	if ts := m.Evaluate(ctx, time.Now().Add(time.Second)); len(ts) != 1 || ts[0].To != "" {
		t.Fatalf("recovery transitions = %+v, want one to \"\"", ts)
	}
	if got := record(t, ctx, st, "n1").State; got != "" {
		t.Fatalf("state = %q, want cleared after fresh status", got)
	}
}

// TestListReportsLifecycleAndLastSeen covers the node listing every CLI shows.
func TestListReportsLifecycleAndLastSeen(t *testing.T) {
	st, ctx := newLCEnv(t)
	writeNode(t, ctx, st, "n1", "")
	writeNode(t, ctx, st, "n2", "witness")
	writeStatus(t, ctx, st, "n1")
	if _, err := st.Put(ctx, store.Key(join.NodesKeyPrefix+"n2/status"), []byte("health=bad degraded=true")); err != nil {
		t.Fatal(err)
	}
	if err := nodelc.Cordon(ctx, st, "n2"); err != nil {
		t.Fatal(err)
	}

	got, err := nodelc.List(ctx, st)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List = %d nodes, want 2: %+v", len(got), got)
	}
	n1, n2 := got[0], got[1]
	if n1.ID != "n1" || n1.Lifecycle != "healthy" || n1.Cordoned || n1.LastSeen.IsZero() {
		t.Errorf("n1 = %+v, want healthy, uncordoned, seen", n1)
	}
	if n2.ID != "n2" || n2.Role != "witness" || n2.Lifecycle != "healthy/degraded" || !n2.Cordoned {
		t.Errorf("n2 = %+v, want cordoned healthy/degraded witness", n2)
	}
}

// TestMonitorFinishesAnInterruptedRemoval: a node that removed itself
// loses its store once raft drops it, so the leader's monitor finishes
// the cleanup for any revoked node still on record.
func TestMonitorFinishesAnInterruptedRemoval(t *testing.T) {
	st, ctx := newLCEnv(t)
	writeNode(t, ctx, st, "n0", "")
	writeNode(t, ctx, st, "n3", "")
	if _, err := st.Put(ctx, store.Key(nodelc.RevokedKeyPrefix+"n3"), []byte(`{"node_id":"n3"}`)); err != nil {
		t.Fatal(err)
	}
	(&nodelc.Monitor{St: st, ThisNodeID: "n0"}).Evaluate(ctx, time.Now())
	if _, err := st.Get(ctx, store.Key(join.NodesKeyPrefix+"n3")); !errors.Is(err, errors.KindNotFound) {
		t.Errorf("revoked n3 still on record: %v", err)
	}
	if _, err := st.Get(ctx, store.Key(join.NodesKeyPrefix+"n0")); err != nil {
		t.Errorf("n0 record lost: %v", err)
	}
}
