package web

import (
	"context"
	"net/http"
	"sync"
	"time"

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
	// Stale: Watch reads the local FSM, and a stream must outlive a lost leader.
	cur, err := st.Revision(store.WithStale(ctx))
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

// streamFragments serves one fragment as SSE: rendered now, after each burst
// of changes and on every tick (0 for none), and sent only when it differs.
// render returns "" to skip a transient failure; watch before calling it.
func streamFragments(w http.ResponseWriter, r *http.Request, event string, changes <-chan struct{}, tick time.Duration, render func(context.Context) (string, error)) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // confirm the stream is live before the first event

	var last string
	send := func() error {
		frag, err := render(ctx)
		if err != nil || frag == "" || frag == last {
			return err // an identical swap would only redraw the page
		}
		last = frag
		if err := writeSSE(w, event, frag); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	if err := send(); err != nil {
		return
	}

	var ticks <-chan time.Time
	if tick > 0 {
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		select {
		case _, ok := <-changes:
			if !ok {
				return // the watch ended; EventSource reconnects
			}
			coalesce(ctx, changes, 250*time.Millisecond)
		case <-ticks:
		case <-ctx.Done():
			return
		}
		if err := send(); err != nil {
			return
		}
	}
}

// coalesce drains further change signals for a short window so a burst
// of writes produces one re-render, not one per write.
func coalesce(ctx context.Context, changes <-chan struct{}, window time.Duration) {
	deadline := time.After(window)
	for {
		select {
		case _, ok := <-changes:
			if !ok {
				return
			}
		case <-deadline:
			return
		case <-ctx.Done():
			return
		}
	}
}
