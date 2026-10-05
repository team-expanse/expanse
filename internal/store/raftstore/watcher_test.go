package raftstore

import (
	"context"
	"testing"

	"github.com/expanse/expanse/internal/store"
)

func TestAWatcherOverflowedMidBatchIsClosedWithoutPanicking(t *testing.T) {
	w := newWatchBroadcaster()
	ch := w.watch(context.Background(), "/a/", 0)
	events := make([]store.Event, watchBuffer+2) // one commit larger than the buffer
	for i := range events {
		events[i] = store.Event{Revision: store.Revision(i + 1), Entry: &store.Entry{Key: "/a/k"}}
	}
	w.broadcast(events)
	n := 0
	for range ch {
		n++
	}
	if n != watchBuffer {
		t.Errorf("received %d events before the close, want %d", n, watchBuffer)
	}
}
