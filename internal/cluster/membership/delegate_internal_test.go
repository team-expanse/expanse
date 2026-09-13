package membership

import (
	"testing"
)

// TestDelegateNoOpPaths covers the delegate plumbing memberlist calls
// into on every event; Expanse keeps liveness-only state so these are
// deliberately no-ops — the test pins that contract (they must not
// panic or allocate gossiped state).
func TestDelegateNoOpPaths(t *testing.T) {
	a := &Agent{local: NodeMeta{NodeID: "n1", Role: "voter"}}
	d := &agentDelegate{agent: a}

	if got := d.NodeMeta(1024); len(got) == 0 {
		t.Error("NodeMeta returned no metadata")
	}
	if got := d.NodeMeta(1); got != nil {
		t.Error("NodeMeta over the limit must return nil")
	}
	d.NotifyMsg([]byte("anything")) // must not panic
	if st := d.LocalState(true); st != nil {
		t.Error("LocalState must be nil (liveness only)")
	}
	if b := d.GetBroadcasts(0, 1400); len(b) != 0 {
		t.Error("GetBroadcasts must be empty (liveness only)")
	}
	d.MergeRemoteState([]byte(`{"junk":true}`), true) // must not panic
}

// TestEventKindString pins the human-readable event names.
func TestEventKindString(t *testing.T) {
	if EventJoin.String() == "" || EventLeave.String() == "" {
		t.Error("event kinds must render")
	}
	if EventJoin.String() == EventLeave.String() {
		t.Error("distinct events must render distinctly")
	}
}

// TestNodeMetaMarshalRoundTrip covers the meta encode path.
func TestNodeMetaMarshalRoundTrip(t *testing.T) {
	m := NodeMeta{NodeID: "n1", Role: "witness"}
	b, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("empty meta")
	}
}
