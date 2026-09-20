package runtime

import (
	"fmt"
	"sync"
	"time"

	"github.com/expanse/expanse/internal/storage/exvol/recovery"
	"github.com/expanse/expanse/internal/storage/exvol/transport"
	pb "github.com/expanse/expanse/proto"
)

// probeTimeout bounds one recovery probe of a peer.
const probeTimeout = 5 * time.Second

// querySeqWithin is conn.QuerySeq with a deadline. transport.Conn takes no
// context, so a peer that never answers is cut off by closing the connection:
// recovery, and the volume behind it, must not wait on a silent replica.
func querySeqWithin(conn *transport.Conn, volID string, d time.Duration) (*pb.SeqQueryReply, error) {
	type answer struct {
		rep *pb.SeqQueryReply
		err error
	}
	ch := make(chan answer, 1)
	go func() {
		rep, err := conn.QuerySeq(volID)
		ch <- answer{rep, err}
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case a := <-ch:
		return a.rep, a.err
	case <-t.C:
		_ = conn.Close()
		return nil, fmt.Errorf("query seq: no answer within %v", d)
	}
}

// DefaultTransientRecoveryWindow is how long recovery may keep failing on
// transport errors alone before the volume is treated as unfillable.
const DefaultTransientRecoveryWindow = time.Minute

// classifyFetchErr marks a failed FetchOps as a transport failure when the
// connection itself broke. An error the holder answered with, or anything
// unrecognised, stays definitive (the stricter path).
func classifyFetchErr(err error) error {
	if transport.IsConnFailure(err) {
		return &recovery.TransportError{Err: err}
	}
	return err
}

// transientTracker decides when a volume's recovery has been failing on
// transport errors for so long that it can no longer be called a blip: a
// holder that keeps answering probes but never serves is indistinguishable
// from a dishonest one, so after the window the failure is treated as
// definitive (the original §4.3 4b safeguard).
type transientTracker struct {
	window time.Duration

	mu sync.Mutex
	by map[string]failureSpan
}

type failureSpan struct{ first, last time.Time }

func newTransientTracker(window time.Duration) *transientTracker {
	if window <= 0 {
		window = DefaultTransientRecoveryWindow
	}
	return &transientTracker{window: window, by: map[string]failureSpan{}}
}

// note records a transient failure at now and reports whether failures have
// persisted for the whole window. A long gap since the previous failure
// starts a fresh window.
func (t *transientTracker) note(vol string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.by[vol]
	if !ok || now.Sub(s.last) > 2*t.window {
		s = failureSpan{first: now}
	}
	s.last = now
	t.by[vol] = s
	return now.Sub(s.first) >= t.window
}

// clear forgets vol's failures (recovery succeeded, or the volume latched).
func (t *transientTracker) clear(vol string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.by, vol)
}
