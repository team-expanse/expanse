// The events page: a live log of store writes, the same change stream
// every SSE page already watches, kept in a small in-memory ring so
// the dashboard has "recent events" and an operator opening /events
// sees what just happened, not only what happens next.
package web

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/expanse/expanse/internal/store"
)

// eventLogSize bounds the ring; a burst of reconciler writes is
// hundreds of entries, not thousands.
const eventLogSize = 500

// eventKinds is the filter menu, in display order.
var eventKinds = []string{"block", "volume", "node", "cluster", "resource", "lease", "auth", "other"}

type eventEntry struct {
	At   time.Time
	Verb string // put | delete
	Kind string // one of eventKinds
	Key  string // the key as shown (session IDs redacted)
}

// eventLog is a fixed-size ring of the most recent store events plus
// subscribers for the live stream.
type eventLog struct {
	mu   sync.Mutex
	ring []eventEntry
	next int
	full bool
	subs map[chan eventEntry]struct{}
}

func newEventLog(size int) *eventLog {
	return &eventLog{ring: make([]eventEntry, size), subs: map[chan eventEntry]struct{}{}}
}

func (l *eventLog) add(e eventEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ring[l.next] = e
	l.next = (l.next + 1) % len(l.ring)
	if l.next == 0 {
		l.full = true
	}
	for ch := range l.subs {
		select {
		case ch <- e:
		default: // a slow stream drops rather than blocks the recorder
		}
	}
}

// recent returns up to limit entries newest first, filtered by kind
// when kind is non-empty.
func (l *eventLog) recent(kind string, limit int) []eventEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.next
	if l.full {
		n = len(l.ring)
	}
	var out []eventEntry
	for i := 1; i <= n && len(out) < limit; i++ {
		e := l.ring[(l.next-i+len(l.ring))%len(l.ring)]
		if kind == "" || e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func (l *eventLog) subscribe() chan eventEntry {
	ch := make(chan eventEntry, 64)
	l.mu.Lock()
	l.subs[ch] = struct{}{}
	l.mu.Unlock()
	return ch
}

func (l *eventLog) unsubscribe(ch chan eventEntry) {
	l.mu.Lock()
	delete(l.subs, ch)
	l.mu.Unlock()
}

// run records every store write until ctx ends, re-watching whenever
// the store closes the channel (watcher buffer overflow).
func (l *eventLog) run(ctx context.Context, st store.Store) {
	for ctx.Err() == nil {
		rev, err := st.Revision(ctx)
		if err != nil {
			sleepCtx(ctx, time.Second)
			continue
		}
		ch, err := st.Watch(ctx, store.Key("/"), rev)
		if err != nil {
			sleepCtx(ctx, time.Second)
			continue
		}
		for ev := range ch {
			l.record(ev)
		}
		sleepCtx(ctx, time.Second) // a store that keeps closing the watch must not spin
	}
}

// record adds ev unless it is noise: a health probe, a replica probe's heartbeat, or a put that
// rewrote the same value.
func (l *eventLog) record(ev store.Event) {
	if ev.Entry != nil && (isNodeHealthProbe(string(ev.Entry.Key)) || isProbeHeartbeat(ev.Entry)) {
		return
	}
	if ev.Type == store.EventPut && ev.Prev != nil && ev.Entry != nil && bytes.Equal(ev.Prev.Value, ev.Entry.Value) {
		return
	}
	l.add(entryFor(ev))
}

// isNodeHealthProbe matches the store check's own test writes (/nodes/<id>/health/...).
func isNodeHealthProbe(key string) bool {
	rest, ok := strings.CutPrefix(key, "/nodes/")
	return ok && strings.Contains(rest, "/health/")
}

// isProbeHeartbeat matches a replica readiness record's periodic rewrite of an unchanged result.
func isProbeHeartbeat(e *store.Entry) bool {
	return strings.Contains(string(e.Key), "/status/replicas/") && bytes.Contains(e.Value, []byte(`"heartbeat":true`))
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func entryFor(ev store.Event) eventEntry {
	key := ""
	if ev.Entry != nil {
		key = string(ev.Entry.Key)
	}
	verb := "put"
	if ev.Type == store.EventDelete {
		verb = "delete"
	}
	kind, shown := classifyEventKey(key)
	return eventEntry{At: time.Now(), Verb: verb, Kind: kind, Key: shown}
}

// classifyEventKey maps a key to its display kind and redacts the one
// prefix whose keys are themselves bearer credentials (session IDs).
func classifyEventKey(key string) (kind, shown string) {
	switch {
	case strings.HasPrefix(key, "/blocks/"):
		return "block", key
	case strings.HasPrefix(key, "/volumes/"):
		return "volume", key
	case strings.HasPrefix(key, "/nodes/"):
		return "node", key
	case strings.HasPrefix(key, "/cluster/"), strings.HasPrefix(key, "/generations/"):
		return "cluster", key
	case strings.HasPrefix(key, "/node/"):
		return "resource", key
	case strings.HasPrefix(key, "/leases/"):
		return "lease", key
	case strings.HasPrefix(key, "/ui/sessions/"):
		return "auth", "/ui/sessions/…"
	case strings.HasPrefix(key, "/ui/"):
		return "auth", key
	default:
		return "other", key
	}
}

func (s *Server) registerEventRoutes() {
	s.routes.Handle("GET /events", s.requireAuth(http.HandlerFunc(s.handleEvents)))
	s.routes.Handle("GET /events/stream", s.requireAuth(http.HandlerFunc(s.handleEventsStream)))
}

type eventsData struct {
	Page   page
	Kind   string
	Kinds  []string
	Events []eventEntry
}

// eventKind validates the ?type= filter against the known kinds.
func eventKind(r *http.Request) string {
	want := r.URL.Query().Get("type")
	for _, k := range eventKinds {
		if k == want {
			return k
		}
	}
	return ""
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	kind := eventKind(r)
	s.render(w, http.StatusOK, "events.html", eventsData{
		Page: s.newPage(w, r, "Events", "events", crumb{Label: "Events"}),
		Kind: kind, Kinds: eventKinds, Events: s.events.recent(kind, 200),
	})
}

// handleEventsStream pushes each new event as one rendered table row
// (hx-swap="afterbegin" on the client), honouring the same type filter
// as the page.
func (s *Server) handleEventsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	kind := eventKind(r)
	ch := s.events.subscribe()
	defer s.events.unsubscribe(ch)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			if kind != "" && e.Kind != kind {
				continue
			}
			var buf bytes.Buffer
			if err := s.renderFragment(&buf, "event-row", e); err != nil {
				return
			}
			if err := writeSSE(w, "entry", buf.String()); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
