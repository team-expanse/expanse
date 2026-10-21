package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
)

func reportStorage(t *testing.T, st store.Store, node string, s storage.NodeStorage) {
	t.Helper()
	if err := storage.SaveNodeStorage(context.Background(), st, node, s); err != nil {
		t.Fatal(err)
	}
}

func TestNodeStorageEnable(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	c, csrf := loggedInClient(t, srv, pw)
	reportStorage(t, st, "n1", storage.NodeStorage{State: storage.NodeStorageOff, VG: "expanse", VGSize: 100 << 30, VGFree: 100 << 30})

	_, body := get(t, c, srv.URL+"/nodes/n1")
	for _, want := range []string{`id="storage"`, "Enable storage", `name="percent" value="95"`, "100 GiB"} {
		if !strings.Contains(body, want) {
			t.Errorf("storage card lacks %q", want)
		}
	}

	resp, body := postForm(t, c, srv.URL+"/nodes/n1/storage", csrf, url.Values{"percent": {"120"}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "1 to 95%") {
		t.Errorf("percent 120: status %d, want 400 with the range", resp.StatusCode)
	}
	if _, ok, _ := storage.LoadEnableRequest(context.Background(), st, "n1"); ok {
		t.Fatal("a rejected request was stored")
	}

	resp, body = postForm(t, c, srv.URL+"/nodes/n1/storage", csrf, url.Values{"percent": {"80"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("enable: status %d:\n%s", resp.StatusCode, alerts(body))
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/nodes/n1") {
		t.Errorf("redirect to %q", loc)
	}
	req, ok, _ := storage.LoadEnableRequest(context.Background(), st, "n1")
	if !ok || req != (storage.EnableRequest{VG: "expanse", Pool: "pool", Percent: 80}) {
		t.Fatalf("stored %+v ok=%v", req, ok)
	}
	_, body = get(t, c, srv.URL+"/nodes/n1")
	if !strings.Contains(body, "Turning on") || strings.Contains(body, "Enable storage</button>") {
		t.Error("a pending request still offers the button, or does not say it is turning on")
	}
}

func TestNodeStorageCardStates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rep     storage.NodeStorage
		want    []string
		hasForm bool
	}{
		{"ready from the UI", storage.NodeStorage{State: storage.NodeStorageReady, Source: storage.SourceUI, VG: "expanse", Pool: "pool", PoolSize: 95 << 30, VGSize: 100 << 30, VGFree: 5 << 30}, []string{"On", "95 GiB"}, false},
		{"ready from the config", storage.NodeStorage{State: storage.NodeStorageReady, Source: storage.SourceConfig, VG: "data", Pool: "thin"}, []string{"NixOS configuration", "data/thin"}, false},
		{"reboot", storage.NodeStorage{State: storage.NodeStorageReboot, Source: storage.SourceUI, VG: "expanse", Pool: "pool"}, []string{"Reboot n1"}, false},
		{"failed", storage.NodeStorage{State: storage.NodeStorageFailed, Source: storage.SourceUI, VG: "expanse", Pool: "pool", VGFree: 1 << 30, Message: "creating the thin pool: Insufficient free space"}, []string{"Insufficient free space"}, true},
		{"no VG", storage.NodeStorage{State: storage.NodeStorageOff, VG: "expanse", Message: `this node has no volume group "expanse"`}, []string{"no volume group"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, pw, st := newClusterTestServer(t)
			c, _ := loggedInClient(t, srv, pw)
			reportStorage(t, st, "n1", tc.rep)
			_, body := get(t, c, srv.URL+"/nodes/n1")
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("card lacks %q", want)
				}
			}
			if got := strings.Contains(body, `action="/nodes/n1/storage"`); got != tc.hasForm {
				t.Errorf("enable form shown = %v, want %v", got, tc.hasForm)
			}
		})
	}
}

func TestNodeStorageEnableRefusesANodeWithoutAVG(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	c, csrf := loggedInClient(t, srv, pw)
	reportStorage(t, st, "n1", storage.NodeStorage{State: storage.NodeStorageOff, VG: "expanse", Message: "no volume group"})
	if resp, _ := postForm(t, c, srv.URL+"/nodes/n1/storage", csrf, url.Values{"percent": {"95"}}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
	if resp, _ := postForm(t, c, srv.URL+"/nodes/ghost/storage", csrf, url.Values{"percent": {"95"}}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown node: status %d, want 404", resp.StatusCode)
	}
}

func TestDeployFormSaysWhenNoNodeHasStorage(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "kvm")
	c, _ := loggedInClient(t, srv, pw)
	reportStorage(t, st, "n1", storage.NodeStorage{State: storage.NodeStorageOff, VG: "expanse", VGFree: 1 << 30})

	_, body := get(t, c, srv.URL+"/deploy/vm/instance")
	if !strings.Contains(body, "No node has storage turned on") {
		t.Error("vm/instance form does not warn that storage is off")
	}
	_, body = get(t, c, srv.URL+"/deploy/web/nginx")
	if strings.Contains(body, "No node has storage turned on") {
		t.Error("a type without storage warns about storage")
	}
	reportStorage(t, st, "n1", storage.NodeStorage{State: storage.NodeStorageReady, VG: "expanse", Pool: "pool"})
	_, body = get(t, c, srv.URL+"/deploy/vm/instance")
	if strings.Contains(body, "No node has storage turned on") {
		t.Error("warns although n1 has storage")
	}
}

// A refused enable says why on the page, and a repeat of a queued request is not an error.
func TestNodeStorageEnableExplainsARefusal(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	c, csrf := loggedInClient(t, srv, pw)
	reportStorage(t, st, "n1", storage.NodeStorage{State: storage.NodeStorageOff, VG: "expanse", Message: "no volume group"})
	if _, body := postForm(t, c, srv.URL+"/nodes/n1/storage", csrf, url.Values{"percent": {"95"}}); !strings.Contains(body, "no free volume group") {
		t.Error("the refusal is not shown on the page")
	}
	reportStorage(t, st, "n1", storage.NodeStorage{State: storage.NodeStorageReady, VG: "expanse", Pool: "pool", VGFree: 1 << 30})
	if _, body := postForm(t, c, srv.URL+"/nodes/n1/storage", csrf, url.Values{"percent": {"95"}}); !strings.Contains(body, "Storage is already on") {
		t.Error("enabling storage that is on does not say so")
	}
	reportStorage(t, st, "n1", storage.NodeStorage{State: storage.NodeStorageOff, VG: "expanse", VGSize: 100 << 30, VGFree: 100 << 30})
	postForm(t, c, srv.URL+"/nodes/n1/storage", csrf, url.Values{"percent": {"80"}})
	if resp, _ := postForm(t, c, srv.URL+"/nodes/n1/storage", csrf, url.Values{"percent": {"80"}}); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("a repeated submit: status %d, want 303 back to the node", resp.StatusCode)
	}
}
