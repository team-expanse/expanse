package web

import (
	"context"
	"sync"

	"github.com/expanse/expanse/internal/store"
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

// fanInWatches merges store.Watch channels for several prefixes into one
// change signal, for a page whose render depends on more than one part
// of the key space (cluster.go's original two-prefix version,
// generalized here for health.go's four). Every prefix watches from the
// same starting revision, so a write landing between calls is never
// missed by one prefix and caught by another watching from later.
func fanInWatches(ctx context.Context, st store.Store, prefixes ...store.Key) (<-chan struct{}, error) {
	cur, err := st.Revision(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan struct{})
	var wg sync.WaitGroup
	for _, p := range prefixes {
		ch, err := st.Watch(ctx, p, cur)
		if err != nil {
			return nil, err
		}
		wg.Add(1)
		go func(ch <-chan store.Event) {
			defer wg.Done()
			for range ch {
				select {
				case out <- struct{}{}:
				case <-ctx.Done():
					return
				}
			}
		}(ch)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out, nil
}
