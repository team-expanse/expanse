package proxy

import (
	"testing"
	"time"
)

func backends(ids ...string) []Backend {
	out := make([]Backend, len(ids))
	for i, id := range ids {
		out[i] = Backend{ReplicaIndex: int32(i), NodeID: id, Healthy: true}
	}
	return out
}

func nodeIDs(bs []Backend) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.NodeID
	}
	return out
}

func TestEjectorSkipsAnEjectedBackend(t *testing.T) {
	var e ejector
	all := backends("n1", "n2", "n3")
	e.eject(all[0])
	got := nodeIDs(e.live(all))
	if len(got) != 2 || got[0] != "n2" || got[1] != "n3" {
		t.Fatalf("live = %v, want [n2 n3]", got)
	}
}

func TestEjectorFailsOpenWhenEveryBackendIsEjected(t *testing.T) {
	var e ejector
	all := backends("n1")
	e.eject(all[0])
	if got := nodeIDs(e.live(all)); len(got) != 1 || got[0] != "n1" {
		t.Fatalf("live = %v, want [n1]: with nothing else left, an ejected backend is still worth a try", got)
	}
}

func TestEjectorReadmitsABackendAfterTheEjectionPeriod(t *testing.T) {
	now := time.Unix(1000, 0)
	e := ejector{For: 10 * time.Second, now: func() time.Time { return now }}
	all := backends("n1", "n2")
	e.eject(all[0])
	now = now.Add(9 * time.Second)
	if got := e.live(all); len(got) != 1 {
		t.Fatalf("live = %v at 9s, want n1 still ejected", nodeIDs(got))
	}
	now = now.Add(2 * time.Second)
	if got := e.live(all); len(got) != 2 {
		t.Fatalf("live = %v at 11s, want n1 readmitted", nodeIDs(got))
	}
}

func TestEjectorReadmitsABackendThatConnected(t *testing.T) {
	var e ejector
	all := backends("n1", "n2")
	e.eject(all[0])
	e.readmit(all[0])
	if got := e.live(all); len(got) != 2 {
		t.Fatalf("live = %v, want both after a successful connect", nodeIDs(got))
	}
}
