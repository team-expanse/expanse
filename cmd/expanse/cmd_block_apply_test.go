package main

import (
	"context"
	"errors"
	"testing"

	pb "github.com/expanse/expanse/proto"
	"google.golang.org/grpc"
)

// fakeBlocks has an existing block when exists; Update fails with updateErr.
type fakeBlocks struct {
	pb.BlockServiceClient
	exists    bool
	updateErr error
	created   bool
}

func (f *fakeBlocks) Get(context.Context, *pb.GetBlockRequest, ...grpc.CallOption) (*pb.Block, error) {
	if !f.exists {
		return nil, errors.New("blocks.service.Get: block default/web: not found")
	}
	return &pb.Block{}, nil
}

func (f *fakeBlocks) Update(context.Context, *pb.Block, ...grpc.CallOption) (*pb.Block, error) {
	return nil, f.updateErr
}

func (f *fakeBlocks) Create(context.Context, *pb.Block, ...grpc.CallOption) (*pb.Block, error) {
	f.created = true
	return &pb.Block{}, nil
}

func webBlock() *pb.Block {
	return &pb.Block{Metadata: &pb.Metadata{Name: "web", Namespace: "default"}}
}

func TestApplyReportsWhyAnUpdateWasRefused(t *testing.T) {
	f := &fakeBlocks{exists: true, updateErr: errors.New("InsufficientCPU: node n1 has 0 cpu free")}
	verb, err := applyBlock(context.Background(), f, webBlock())
	if err == nil || err.Error() != "update: InsufficientCPU: node n1 has 0 cpu free" || f.created {
		t.Errorf("verb %q, err %v, created %t: want the update's own error and no create", verb, err, f.created)
	}
}

func TestApplyCreatesABlockThatDoesNotExist(t *testing.T) {
	f := &fakeBlocks{}
	if verb, err := applyBlock(context.Background(), f, webBlock()); err != nil || verb != "created" || !f.created {
		t.Errorf("verb %q, err %v, created %t", verb, err, f.created)
	}
}

func TestApplyUpdatesABlockThatExists(t *testing.T) {
	f := &fakeBlocks{exists: true}
	if verb, err := applyBlock(context.Background(), f, webBlock()); err != nil || verb != "updated" || f.created {
		t.Errorf("verb %q, err %v, created %t", verb, err, f.created)
	}
}
