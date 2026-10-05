package raftstore

import (
	"bytes"
	"context"
	"sync"

	"github.com/expanse/expanse/internal/store"
)

const watchBuffer = 1024 // events per watcher before the channel is closed

// watchBroadcaster fans committed events out to registered watchers with
// the same semantics as boltstore: literal prefix match, revision-order
// delivery, close-on-overflow (never a silent drop), close on ctx cancel
// or store close.
type watchBroadcaster struct {
	mu       sync.Mutex
	watchers map[*watcher]struct{}
}

type watcher struct {
	prefix []byte
	from   store.Revision
	ch     chan store.Event
	closed bool
	cancel context.CancelFunc
}

func newWatchBroadcaster() *watchBroadcaster {
	return &watchBroadcaster{watchers: map[*watcher]struct{}{}}
}

// watch registers a watcher under prefix; events with revision <= fromRev
// are not delivered.
func (w *watchBroadcaster) watch(ctx context.Context, prefix store.Key, fromRev store.Revision) <-chan store.Event {
	wctx, cancel := context.WithCancel(ctx)
	wd := &watcher{
		prefix: []byte(prefix),
		from:   fromRev,
		ch:     make(chan store.Event, watchBuffer),
		cancel: cancel,
	}
	w.mu.Lock()
	w.watchers[wd] = struct{}{}
	w.mu.Unlock()
	go func() {
		<-wctx.Done()
		w.unregister(wd)
	}()
	return wd.ch
}

func (w *watchBroadcaster) unregister(wd *watcher) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.watchers[wd]; !ok {
		return
	}
	delete(w.watchers, wd)
	wd.cancel()
	if !wd.closed {
		wd.closed = true
		close(wd.ch)
	}
}

// broadcast delivers events to matching watchers. Called after the FSM has
// committed the revision (single-threaded from Apply, so events are always
// in revision order).
func (w *watchBroadcaster) broadcast(events []store.Event) {
	if len(events) == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for wd := range w.watchers {
		if wd.closed {
			continue
		}
	events:
		for _, ev := range events {
			var key []byte
			if ev.Entry != nil {
				key = []byte(ev.Entry.Key)
			}
			if !bytes.HasPrefix(key, wd.prefix) || ev.Revision <= wd.from {
				continue
			}
			select {
			case wd.ch <- ev:
			default:
				// Overflow: close rather than drop. Callers must re-list
				// and re-watch.
				wd.closed = true
				close(wd.ch)
				delete(w.watchers, wd)
				break events
			}
		}
	}
}

// close closes every watcher channel (store shutdown).
func (w *watchBroadcaster) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for wd := range w.watchers {
		wd.cancel()
		if !wd.closed {
			wd.closed = true
			close(wd.ch)
		}
		delete(w.watchers, wd)
	}
}
