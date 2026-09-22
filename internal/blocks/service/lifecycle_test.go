package service

import (
	"context"
	"testing"
	"time"

	pb "github.com/expanse/expanse/proto"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// fakeWatchServer adapts pb.BlockService_WatchServer to a channel; only
// Context/Send are ever called by Watch, so the embedded (nil)
// grpc.ServerStream satisfies the rest of the interface unused.
type fakeWatchServer struct {
	grpc.ServerStream
	ctx  context.Context
	recv chan *pb.BlockEvent
}

func (f *fakeWatchServer) Context() context.Context    { return f.ctx }
func (f *fakeWatchServer) Send(e *pb.BlockEvent) error { f.recv <- e; return nil }

// TestWatchObservesStatusOnlyPromotion: the placement controller
// promotes a block to RUNNING by writing only its "/status" sub-key
// (controller.RuntimePass), never the block's own key — Watch, filtered
// by name, must still surface that change, not just Create/Update/Scale/
// Delete's direct writes to the bare key.
func TestWatchObservesStatusOnlyPromotion(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)

	b := &pb.Block{
		Metadata: &pb.Metadata{Name: "web", Namespace: "default"},
		Spec:     &pb.BlockSpec{Type: "util/echo"},
	}
	if _, err := s.Create(ctx, b); err != nil {
		t.Fatalf("Create: %v", err)
	}

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	recv := make(chan *pb.BlockEvent, 4)
	go func() {
		_ = s.Watch(&pb.WatchBlocksRequest{Namespace: "default", Name: "web"}, &fakeWatchServer{ctx: watchCtx, recv: recv})
	}()
	time.Sleep(50 * time.Millisecond) // let Watch's goroutine start listening

	status := &pb.BlockStatus{Phase: pb.Phase_RUNNING}
	raw, err := proto.Marshal(status)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if _, err := s.St.Put(ctx, key("default", "web")+"/status", raw); err != nil {
		t.Fatalf("put status sub-key: %v", err)
	}

	select {
	case ev := <-recv:
		if got := ev.GetBlock().GetStatus().GetPhase(); got != pb.Phase_RUNNING {
			t.Errorf("watch event phase = %v, want RUNNING: %+v", got, ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch never delivered the status-only promotion event")
	}
}
