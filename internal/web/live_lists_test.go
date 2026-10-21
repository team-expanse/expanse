package web

import (
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/storage"
)

// A block deployed while the list is open appears without a reload.
func TestBlocksListUpdatesLive(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "")
	c, csrf := loggedInClient(t, srv, pw)
	if _, body := get(t, c, srv.URL+"/blocks"); !strings.Contains(body, `sse-connect="/blocks/events"`) {
		t.Error("the blocks list is not a live region")
	}
	sse := openSSE(t, c, srv.URL+"/blocks/events")
	sse.expect("No blocks deployed yet", 2*time.Second)
	deployNginx(t, c, srv.URL, csrf)
	sse.expect(`href="/blocks/default/site"`, 2*time.Second)
}

// A volume created while the list is open appears without a reload.
func TestVolumesListUpdatesLive(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	c, _ := loggedInClient(t, srv, pw)
	if _, body := get(t, c, srv.URL+"/volumes"); !strings.Contains(body, `sse-connect="/volumes/_events"`) {
		t.Error("the volumes list is not a live region")
	}
	sse := openSSE(t, c, srv.URL+"/volumes/_events")
	sse.expect("No volumes yet", 2*time.Second)
	putTestVolume(t, st, storage.Spec{ID: "vol-1", Name: "data", SizeBytes: 1 << 30, Class: "default", Replication: 1}, storage.Status{State: storage.StateHealthy})
	sse.expect(`href="/volumes/data"`, 2*time.Second)
}
