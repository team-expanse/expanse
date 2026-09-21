package storage

import (
	"fmt"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
)

func drbdClass(rep int, selector map[string]string) StorageClass {
	return StorageClass{Name: "default", Driver: "drbd", Replication: rep, NodeSelector: selector}
}

func node(id string, free uint64, labels map[string]string) NodeInfo {
	return NodeInfo{ID: id, FreeBytes: free, Labels: labels}
}

func ids(ns []NodeInfo) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.ID
	}
	return out
}

func TestSelectNodesEvenSpreadDistinctNodes(t *testing.T) {
	class := drbdClass(3, nil)
	nodes := []NodeInfo{
		node("n1", 100<<30, nil),
		node("n2", 100<<30, nil),
		node("n3", 100<<30, nil),
		node("n4", 100<<30, nil),
		node("n5", 100<<30, nil),
	}
	got, err := SelectNodes(class, nodes, nil)
	if err != nil {
		t.Fatalf("SelectNodes: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d nodes, want 3", len(got))
	}
	seen := map[string]bool{}
	for _, n := range got {
		if seen[n.ID] {
			t.Errorf("node %s selected twice", n.ID)
		}
		seen[n.ID] = true
	}
}

func TestSelectNodesPrefersMoreFreeSpace(t *testing.T) {
	class := drbdClass(3, nil)
	nodes := []NodeInfo{
		node("n1", 10<<30, nil),
		node("n2", 900<<30, nil),
		node("n3", 500<<30, nil),
		node("n4", 5<<30, nil),
		node("n5", 100<<30, nil),
	}
	got, err := SelectNodes(class, nodes, nil)
	if err != nil {
		t.Fatalf("SelectNodes: %v", err)
	}
	want := []string{"n2", "n3", "n5"}
	if gotIDs := ids(got); fmt.Sprint(gotIDs) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v (free-space descending)", gotIDs, want)
	}
}

func TestSelectNodesExcludesExistingPlacements(t *testing.T) {
	class := drbdClass(2, nil)
	nodes := []NodeInfo{
		node("n1", 900<<30, nil),
		node("n2", 800<<30, nil),
		node("n3", 700<<30, nil),
		node("n4", 600<<30, nil),
	}
	// n1 and n2 already hold replicas of this volume (e.g. after a
	// rebuild); they must not be picked again even though they have the
	// most free space.
	got, err := SelectNodes(class, nodes, []string{"n1", "n2"})
	if err != nil {
		t.Fatalf("SelectNodes: %v", err)
	}
	gotIDs := ids(got)
	if fmt.Sprint(gotIDs) != fmt.Sprint([]string{"n3", "n4"}) {
		t.Errorf("got %v, want [n3 n4]", gotIDs)
	}
}

func TestSelectNodesNodeSelector(t *testing.T) {
	class := drbdClass(2, map[string]string{"disk": "ssd"})
	nodes := []NodeInfo{
		node("n1", 900<<30, map[string]string{"disk": "hdd"}),
		node("n2", 800<<30, map[string]string{"disk": "ssd", "zone": "a"}),
		node("n3", 700<<30, map[string]string{"disk": "ssd"}),
		node("n4", 600<<30, nil),
		node("n5", 500<<30, map[string]string{"disk": "ssd"}),
	}
	got, err := SelectNodes(class, nodes, nil)
	if err != nil {
		t.Fatalf("SelectNodes: %v", err)
	}
	gotIDs := ids(got)
	if fmt.Sprint(gotIDs) != fmt.Sprint([]string{"n2", "n3"}) {
		t.Errorf("got %v, want [n2 n3] (ssd only, free-space order)", gotIDs)
	}
}

func TestSelectNodesInsufficientNodes(t *testing.T) {
	class := drbdClass(3, nil)
	nodes := []NodeInfo{node("n1", 100<<30, nil), node("n2", 100<<30, nil)}
	_, err := SelectNodes(class, nodes, nil)
	if experrors.KindOf(err) != experrors.KindResourceExhausted {
		t.Errorf("KindOf = %v, want resource_exhausted (err: %v)", experrors.KindOf(err), err)
	}
}

func TestSelectNodesInsufficientAfterExclusions(t *testing.T) {
	class := drbdClass(2, nil)
	nodes := []NodeInfo{node("n1", 900<<30, nil), node("n2", 800<<30, nil), node("n3", 700<<30, nil)}
	_, err := SelectNodes(class, nodes, []string{"n1", "n2"})
	if experrors.KindOf(err) != experrors.KindResourceExhausted {
		t.Errorf("KindOf = %v, want resource_exhausted", experrors.KindOf(err))
	}
}

func TestSelectNodesSelectorTooNarrow(t *testing.T) {
	class := drbdClass(3, map[string]string{"disk": "ssd"})
	nodes := []NodeInfo{
		node("n1", 900<<30, map[string]string{"disk": "ssd"}),
		node("n2", 800<<30, map[string]string{"disk": "ssd"}),
		node("n3", 700<<30, map[string]string{"disk": "hdd"}),
	}
	_, err := SelectNodes(class, nodes, nil)
	if experrors.KindOf(err) != experrors.KindResourceExhausted {
		t.Errorf("KindOf = %v, want resource_exhausted", experrors.KindOf(err))
	}
}

func TestSelectNodesUnknownFreeSpaceStillEligible(t *testing.T) {
	class := drbdClass(2, nil)
	nodes := []NodeInfo{
		node("n1", 0, nil), // unknown free space
		node("n2", 100<<30, nil),
		node("n3", 200<<30, nil),
	}
	got, err := SelectNodes(class, nodes, nil)
	if err != nil {
		t.Fatalf("SelectNodes: %v", err)
	}
	gotIDs := ids(got)
	if fmt.Sprint(gotIDs) != fmt.Sprint([]string{"n3", "n2"}) {
		t.Errorf("got %v, want [n3 n2] (unknown space sorts last)", gotIDs)
	}
}

func TestSelectNodesDeterministicTieBreak(t *testing.T) {
	class := drbdClass(2, nil)
	nodes := []NodeInfo{
		node("n3", 100<<30, nil),
		node("n1", 100<<30, nil),
		node("n2", 100<<30, nil),
		node("n5", 100<<30, nil),
		node("n4", 100<<30, nil),
	}
	first, err := SelectNodes(class, nodes, nil)
	if err != nil {
		t.Fatalf("SelectNodes: %v", err)
	}
	for i := 0; i < 20; i++ {
		got, err := SelectNodes(class, nodes, nil)
		if err != nil {
			t.Fatalf("SelectNodes: %v", err)
		}
		if fmt.Sprint(ids(got)) != fmt.Sprint(ids(first)) {
			t.Fatalf("non-deterministic: %v vs %v", ids(got), ids(first))
		}
	}
	// Ties must break by ID: n1 and n2 win deterministically.
	gotIDs := ids(first)
	if fmt.Sprint(gotIDs) != fmt.Sprint([]string{"n1", "n2"}) {
		t.Errorf("got %v, want [n1 n2] (ID tie-break)", gotIDs)
	}
}

func TestSelectNodesInvalidReplication(t *testing.T) {
	for _, rep := range []int{0, -1} {
		_, err := SelectNodes(drbdClass(rep, nil), []NodeInfo{node("n1", 1, nil)}, nil)
		if experrors.KindOf(err) != experrors.KindInvalid {
			t.Errorf("rep=%d KindOf = %v, want invalid", rep, experrors.KindOf(err))
		}
	}
}

func TestSelectNodesReplicationOne(t *testing.T) {
	class := drbdClass(1, nil)
	nodes := []NodeInfo{node("n1", 10<<30, nil), node("n2", 100<<30, nil)}
	got, err := SelectNodes(class, nodes, nil)
	if err != nil {
		t.Fatalf("SelectNodes: %v", err)
	}
	if len(got) != 1 || got[0].ID != "n2" {
		t.Errorf("got %v, want [n2]", ids(got))
	}
}

func TestSelectNodesSelectorEmptyMatchesAll(t *testing.T) {
	class := drbdClass(2, map[string]string{})
	nodes := []NodeInfo{
		node("n1", 100<<30, nil),
		node("n2", 200<<30, map[string]string{"anything": "x"}),
	}
	got, err := SelectNodes(class, nodes, nil)
	if err != nil {
		t.Fatalf("SelectNodes: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("empty selector should match all, got %v", ids(got))
	}
}

func TestSelectNodesMissingID(t *testing.T) {
	class := drbdClass(1, nil)
	nodes := []NodeInfo{{}}
	_, err := SelectNodes(class, nodes, nil)
	if experrors.KindOf(err) != experrors.KindInvalid {
		t.Errorf("KindOf = %v, want invalid", experrors.KindOf(err))
	}
}
