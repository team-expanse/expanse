package web

import (
	"context"

	pb "github.com/expanse/expanse/proto"
	"google.golang.org/grpc"
)

// blockEventStream adapts pb.BlockService_WatchServer to a plain
// callback so the web server can call blocks.Watch in-process (D1) and
// re-emit each event as SSE, without a loopback gRPC dial. Watch only
// ever calls Context and Send; the embedded grpc.ServerStream (left
// nil) satisfies the rest of the interface and is never invoked.
type blockEventStream struct {
	grpc.ServerStream
	ctx  context.Context
	send func(*pb.BlockEvent) error
}

func (s *blockEventStream) Context() context.Context    { return s.ctx }
func (s *blockEventStream) Send(e *pb.BlockEvent) error { return s.send(e) }

// logLineStream is the StreamLogs equivalent of blockEventStream.
type logLineStream struct {
	grpc.ServerStream
	ctx  context.Context
	send func(*pb.LogLine) error
}

func (s *logLineStream) Context() context.Context { return s.ctx }
func (s *logLineStream) Send(l *pb.LogLine) error { return s.send(l) }
