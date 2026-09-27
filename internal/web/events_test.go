package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
)

func TestEventKindClassifiesKeysAndRedactsSessions(t *testing.T) {
	cases := map[string]struct{ kind, shown string }{
		"/blocks/default/web":          {"block", "/blocks/default/web"},
		"/volumes/vol-1/spec":          {"volume", "/volumes/vol-1/spec"},
		"/nodes/n2/status":             {"node", "/nodes/n2/status"},
		"/cluster/generation":          {"cluster", "/cluster/generation"},
		"/generations/5/meta":          {"cluster", "/generations/5/meta"},
		"/node/n1/desired/file:/etc/x": {"resource", "/node/n1/desired/file:/etc/x"},
		"/ui/sessions/deadbeef":        {"auth", "/ui/sessions/…"},
		"/ui/users/admin":              {"auth", "/ui/users/admin"},
		"/leases/block/default/web":    {"lease", "/leases/block/default/web"},
		"/whatever":                    {"other", "/whatever"},
	}
	for key, want := range cases {
		kind, shown := classifyEventKey(key)
		if kind != want.kind || shown != want.shown {
			t.Errorf("classifyEventKey(%q) = %q %q, want %q %q", key, kind, shown, want.kind, want.shown)
		}
	}
}

func TestEventLogKeepsANewestFirstBoundedRing(t *testing.T) {
	l := newEventLog(3)
	for i := 0; i < 5; i++ {
		l.add(eventEntry{Key: string(rune('a' + i))})
	}
	got := l.recent("", 10)
	if len(got) != 3 || got[0].Key != "e" || got[2].Key != "c" {
		t.Errorf("recent = %+v, want e,d,c", got)
	}
	l.add(eventEntry{Key: "/x", Kind: "volume"})
	if got := l.recent("volume", 10); len(got) != 1 || got[0].Key != "/x" {
		t.Errorf("filtered recent = %+v, want just /x", got)
	}
}

func TestEventsPageListsStoreWritesWithFilterAndNeverLeaksSessions(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	sessionID := sessionCookieValue(t, client, srv)

	putTestVolume(t, st,
		storage.Spec{ID: "vol-ev", Name: "evvol", SizeBytes: 1 << 20, Replication: 1},
		storage.Status{State: storage.StateHealthy},
	)
	waitFor(t, 5*time.Second, func() bool {
		_, body := getPage(t, client, srv, "/events")
		return strings.Contains(body, "/volumes/vol-ev")
	})

	resp, body := getPage(t, client, srv, "/events")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "/volumes/vol-ev") || !strings.Contains(body, "/ui/sessions/…") {
		t.Errorf("events page missing the volume write or the redacted session write: %s", body)
	}
	if strings.Contains(body, sessionID) {
		t.Fatal("events page leaks a session ID")
	}

	_, body = getPage(t, client, srv, "/events?type=node")
	if strings.Contains(body, "/volumes/vol-ev") {
		t.Errorf("type filter did not hide volume events: %s", body)
	}
	if !strings.Contains(body, `value="node" selected`) {
		t.Errorf("type filter not reflected in the form: %s", body)
	}
}

func TestEventsStreamSSEAppendsNewWrites(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	sessionID := sessionCookieValue(t, client, srv)

	stream := openSSE(t, client, srv.URL+"/events/stream?type=volume")
	if _, err := st.Put(context.Background(), store.Key("/volumes/vol-s/spec"), []byte("{}")); err != nil {
		t.Fatal(err)
	}
	stream.expect("/volumes/vol-s/spec", 10*time.Second)
	if strings.Contains(stream.got(), sessionID) {
		t.Fatal("events stream leaks a session ID")
	}
	// A node write must be filtered out, but a later volume write still arrives.
	if _, err := st.Put(context.Background(), store.Key("/nodes/zz/status"), []byte("health=healthy")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), store.Key("/volumes/vol-t/spec"), []byte("{}")); err != nil {
		t.Fatal(err)
	}
	stream.expect("/volumes/vol-t/spec", 10*time.Second)
	if strings.Contains(stream.got(), "/nodes/zz/status") {
		t.Error("type filter not applied to the stream")
	}
}

func TestEventLogSkipsProbesAndUnchangedWrites(t *testing.T) {
	put := func(key, prev, val string) store.Event {
		ev := store.Event{Type: store.EventPut, Entry: &store.Entry{Key: store.Key(key), Value: []byte(val)}}
		if prev != "" {
			ev.Prev = &store.Entry{Key: store.Key(key), Value: []byte(prev)}
		}
		return ev
	}
	l := newEventLog(10)
	l.record(put("/nodes/n1/health/store-probe", "1", "2"))
	l.record(put("/nodes/n1/status", "", "health=healthy"))
	l.record(put("/nodes/n1/status", "health=healthy", "health=healthy"))
	l.record(put("/nodes/n1/status", "health=healthy", "health=degraded"))
	l.record(store.Event{Type: store.EventDelete, Entry: &store.Entry{Key: "/nodes/n1/status"}})

	got := l.recent("", 10)
	if len(got) != 3 {
		t.Fatalf("recorded %d events, want 3 (first write, a change, a delete): %+v", len(got), got)
	}
	for _, e := range got {
		if strings.Contains(e.Key, "store-probe") {
			t.Errorf("health probe write recorded: %+v", e)
		}
	}
}

func TestEventLogKeepsABlockNamedHealth(t *testing.T) {
	l := newEventLog(4)
	l.record(store.Event{Type: store.EventPut, Entry: &store.Entry{Key: "/blocks/default/health/spec", Value: []byte("x")}})
	if got := l.recent("", 4); len(got) != 1 {
		t.Errorf("block write under a block named health was dropped: %+v", got)
	}
}
