package console

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/expanse/expanse/proto"
)

func blockGetter(blocks ...*pb.Block) func(context.Context, *pb.GetBlockRequest) (*pb.Block, error) {
	return func(_ context.Context, req *pb.GetBlockRequest) (*pb.Block, error) {
		for _, b := range blocks {
			if b.GetMetadata().GetNamespace() == req.GetNamespace() && b.GetMetadata().GetName() == req.GetName() {
				return b, nil
			}
		}
		return nil, status.Error(codes.NotFound, "no such block")
	}
}

func block(name, typ string, placements ...*pb.PlacementStatus) *pb.Block {
	return &pb.Block{
		Metadata: &pb.Metadata{Namespace: "default", Name: name},
		Spec:     &pb.BlockSpec{Type: typ},
		Status:   &pb.BlockStatus{Placements: placements},
	}
}

func TestLocatorFindsTheNodeRunningAReplica(t *testing.T) {
	locate := Locator(blockGetter(block("vm1", "vm/instance",
		&pb.PlacementStatus{ReplicaIndex: -1, NodeId: "old"},
		&pb.PlacementStatus{ReplicaIndex: 0, NodeId: "n2"},
	)))
	host, err := locate(context.Background(), "default", "vm1", 0)
	if err != nil || host != "n2" {
		t.Fatalf("Locate = %q, %v; want n2", host, err)
	}
}

func TestLocatorRefusesWhatHasNoConsole(t *testing.T) {
	locate := Locator(blockGetter(
		block("web", "web/nginx", &pb.PlacementStatus{ReplicaIndex: 0, NodeId: "n1"}),
		block("vm1", "vm/instance"),
	))
	for _, tc := range []struct {
		name    string
		replica int32
		want    codes.Code
	}{
		{"web", 0, codes.FailedPrecondition},
		{"vm1", 0, codes.Unavailable},
		{"absent", 0, codes.NotFound},
	} {
		if _, err := locate(context.Background(), "default", tc.name, tc.replica); status.Code(err) != tc.want {
			t.Errorf("Locate(%s/%d) error %v, want %v", tc.name, tc.replica, err, tc.want)
		}
	}
}
