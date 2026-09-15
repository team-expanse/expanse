package agent

// Tests for the §4.2 candidate extraction: a placement counts as a VIP
// candidate exactly while its phase is RUNNING. Liveness is not part of
// this gate — a dead holder's failure is proven by its lease expiring
// (Holder.Read of the recorded lease), not by placement bookkeeping.

import (
	"testing"

	pb "github.com/expanse/expanse/proto"
)

func blockWithPlacements(nodes []string, phases ...pb.Phase) *pb.Block {
	b := &pb.Block{Status: &pb.BlockStatus{}}
	for i, n := range nodes {
		phase := pb.Phase_RUNNING
		if i < len(phases) {
			phase = phases[i]
		}
		b.Status.Placements = append(b.Status.Placements,
			&pb.PlacementStatus{ReplicaIndex: int32(i), NodeId: n, Phase: phase})
	}
	return b
}

func TestReadyCandidates(t *testing.T) {
	t.Run("running placements count, once per node", func(t *testing.T) {
		b := blockWithPlacements([]string{"n1", "n2"})
		got := readyCandidates(b)
		if len(got) != 2 {
			t.Fatalf("want 2 candidates, got %+v", got)
		}
	})

	t.Run("non-running placements are skipped", func(t *testing.T) {
		b := blockWithPlacements([]string{"n1", "n2", "n3"},
			pb.Phase_RUNNING, pb.Phase_PENDING, pb.Phase_FAILED)
		got := readyCandidates(b)
		if len(got) != 1 || got[0].NodeID != "n1" {
			t.Fatalf("want only n1, got %+v", got)
		}
	})

	t.Run("no placements means no candidates", func(t *testing.T) {
		if got := readyCandidates(&pb.Block{Status: &pb.BlockStatus{}}); len(got) != 0 {
			t.Fatalf("want no candidates, got %+v", got)
		}
	})
}
