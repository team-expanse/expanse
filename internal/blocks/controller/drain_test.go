package controller

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

func putStatus(t *testing.T, st store.Store, block string, ps ...*pb.PlacementStatus) {
	t.Helper()
	out, err := proto.Marshal(&pb.BlockStatus{Placements: ps})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), store.Key("/blocks/default/"+block+StatusSuffix), out); err != nil {
		t.Fatal(err)
	}
}

func place(idx int32, node string, phase pb.Phase) *pb.PlacementStatus {
	return &pb.PlacementStatus{ReplicaIndex: idx, NodeId: node, Phase: phase}
}

func TestNodeReplicasListsTheLiveReplicasOnANode(t *testing.T) {
	st := newStore(t)
	retired := place(-1, "n1", pb.Phase_TERMINATED)
	putStatus(t, st, "web", place(0, "n1", pb.Phase_RUNNING), place(1, "n2", pb.Phase_RUNNING), retired)
	putStatus(t, st, "db", place(0, "n1", pb.Phase_RUNNING))
	got, err := NodeReplicas(context.Background(), st, "n1")
	if err != nil {
		t.Fatal(err)
	}
	want := []NodeReplica{{"default/db", 0}, {"default/web", 0}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("NodeReplicas = %+v, want %+v", got, want)
	}
}

func TestDrainProgress(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	was := []NodeReplica{{"default/web", 0}, {"default/db", 0}, {"default/api", 0}, {"default/gone", 0}}
	held := place(0, "n1", pb.Phase_RUNNING)
	held.Held = "no other node holds a healthy copy of its volumes"
	putStatus(t, st, "db", held)
	putStatus(t, st, "web", place(-1, "n1", pb.Phase_TERMINATED), place(0, "n2", pb.Phase_STARTING))
	putStatus(t, st, "api", place(0, "n3", pb.Phase_RUNNING))
	moving, heldBy, err := DrainProgress(ctx, st, "n1", was)
	if err != nil {
		t.Fatal(err)
	}
	if len(moving) != 1 || moving[0] != was[0] {
		t.Errorf("moving = %+v, want only web, still starting on n2", moving)
	}
	if heldBy[was[1]] != held.Held || len(heldBy) != 1 {
		t.Errorf("held = %+v, want db with its reason", heldBy)
	}
}
