package recovery

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func answering(id string, last uint64) Probe { return Probe{NodeID: id, Answered: true, LastSeq: last} }
func dead(id string) Probe                   { return Probe{NodeID: id} }

func TestRecoverReportsNoReachableReplicaAsATypedError(t *testing.T) {
	_, err := Recover(context.Background(), []Probe{dead("n1"), answering("n2", 0)}, nil, nil, nil)
	if !errors.Is(err, ErrNoReachableReplica) {
		t.Fatalf("err = %v, want ErrNoReachableReplica", err)
	}
}

// R=3, primary dead, one Stale peer that answers and is behind: everything
// acked lives on the candidate, so it can serve at once while the peer is
// resynced.
func TestSoleCurrentServesWithAStalePeerBehind(t *testing.T) {
	res, ok, why := SoleCurrent([]Probe{dead("n1"), answering("n2", 0)}, "n3", 4)
	if !ok {
		t.Fatalf("must serve: %s", why)
	}
	if res.NewPrimaryID != "n3" || res.MaxSeq != 4 {
		t.Errorf("result = %+v, want primary n3 at seq 4", res)
	}
	if len(res.Stale) != 2 {
		t.Errorf("both peers must be reported Stale so they get resynced: %v", res.Stale)
	}
}

// A Stale peer ahead of the candidate may hold acked ops nobody else has
// (the dead node acked them with it): serving would silently drop them.
func TestSoleCurrentRefusesWhenAStalePeerIsAhead(t *testing.T) {
	_, ok, why := SoleCurrent([]Probe{dead("n1"), answering("n2", 9)}, "n3", 4)
	if ok || !strings.Contains(why, "ahead") {
		t.Fatalf("ok=%v why=%q, want a refusal that says the peer is ahead", ok, why)
	}
}

// With every peer silent the candidate is one copy of three: acked ops
// may exist only on the two it cannot see.
func TestSoleCurrentRefusesWithoutAReadQuorum(t *testing.T) {
	_, ok, why := SoleCurrent([]Probe{dead("n1"), dead("n2")}, "n3", 4)
	if ok || !strings.Contains(why, "read quorum") {
		t.Fatalf("ok=%v why=%q, want a read-quorum refusal", ok, why)
	}
}

func TestSoleCurrentNotApplicableWhenATrustedPeerExists(t *testing.T) {
	trusted := Probe{NodeID: "n1", Reachable: true, LastSeq: 4}
	if _, ok, _ := SoleCurrent([]Probe{trusted, dead("n2")}, "n3", 4); ok {
		t.Fatal("the normal recovery path owns this case")
	}
}

func TestSoleCurrentReadQuorumArithmetic(t *testing.T) {
	cases := []struct {
		name   string
		peers  []Probe
		wantOK bool
	}{
		{"R=2 survivor serves reads (writes need both)", []Probe{dead("n1")}, true},
		{"R=5: candidate + 1 answering is below the read quorum of 3", []Probe{answering("n2", 0), dead("n3"), dead("n4"), dead("n5")}, false},
		{"R=5: candidate + 2 answering meets it", []Probe{answering("n2", 0), answering("n3", 1), dead("n4"), dead("n5")}, true},
	}
	for _, c := range cases {
		if _, ok, why := SoleCurrent(c.peers, "n1", 4); ok != c.wantOK {
			t.Errorf("%s: ok=%v (%s), want %v", c.name, ok, why, c.wantOK)
		}
	}
}
