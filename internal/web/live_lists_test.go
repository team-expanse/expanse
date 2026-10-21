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

// Storage turning on while the node page is open shows without a reload.
func TestNodeDetailUpdatesLive(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	c, _ := loggedInClient(t, srv, pw)
	reportStorage(t, st, "n1", storage.NodeStorage{State: storage.NodeStorageOff, VG: "expanse", VGSize: 100 << 30, VGFree: 100 << 30})
	if _, body := get(t, c, srv.URL+"/nodes/n1"); !strings.Contains(body, `sse-connect="/nodes/n1/events"`) {
		t.Error("the node page is not a live region")
	}
	sse := openSSE(t, c, srv.URL+"/nodes/n1/events")
	sse.expect("Enable storage", 2*time.Second)
	if strings.Contains(sse.got(), `name="csrf_token" value=""`) {
		t.Error("the live storage form has no CSRF token")
	}
	reportStorage(t, st, "n1", storage.NodeStorage{State: storage.NodeStorageReboot, Source: storage.SourceUI, VG: "expanse", Pool: "pool"})
	sse.expect("Reboot n1", 2*time.Second)
}
