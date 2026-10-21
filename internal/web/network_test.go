package web

import (
	"context"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/network/vip"
	"github.com/expanse/expanse/internal/store"
)

// seedNodeAt records a node whose Raft address is on the LAN at ip.
func seedNodeAt(t *testing.T, st store.Store, id, ip string) {
	t.Helper()
	rec := `{"id":"` + id + `","role":"voter","raft_addr":"` + ip + `:7000"}`
	if _, err := st.Put(context.Background(), store.Key("/nodes/"+id), []byte(rec)); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkSettingSavesAndShows(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNodeAt(t, st, "n1", "192.168.1.10")
	c, csrf := loggedInClient(t, srv, pw)

	_, body := get(t, c, srv.URL+"/cluster/settings")
	for _, want := range []string{`id="network"`, `name="externalVIPPool"`, "inside the cluster only"} {
		if !strings.Contains(body, want) {
			t.Errorf("settings missing %q", want)
		}
	}

	resp, body := postForm(t, c, srv.URL+"/cluster/network", csrf, url.Values{"externalVIPPool": {" 192.168.1.100-192.168.1.120 "}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: status %d:\n%s", resp.StatusCode, alerts(body))
	}
	if s, _ := vip.LoadSettings(context.Background(), st); s.ExternalPool != "192.168.1.100-192.168.1.120" {
		t.Errorf("stored %+v", s)
	}
	_, body = get(t, c, srv.URL+"/cluster/settings")
	if !strings.Contains(body, `value="192.168.1.100-192.168.1.120"`) || strings.Contains(body, "inside the cluster only") {
		t.Error("settings page does not show the saved range")
	}

	resp, body = postForm(t, c, srv.URL+"/cluster/network", csrf, url.Values{"externalVIPPool": {"10.0.0.5-10.0.0.9"}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "No node is on 10.0.0.0/24") {
		t.Errorf("a range off the nodes' subnet: status %d, body lacks the reason", resp.StatusCode)
	}
	if !strings.Contains(body, `value="10.0.0.5-10.0.0.9"`) {
		t.Error("the rejected input is not kept for correction")
	}
	if s, _ := vip.LoadSettings(context.Background(), st); s.ExternalPool != "192.168.1.100-192.168.1.120" {
		t.Errorf("a rejected range was stored: %+v", s)
	}

	if resp, _ := postForm(t, c, srv.URL+"/cluster/network", csrf, url.Values{"externalVIPPool": {""}}); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("clear: status %d", resp.StatusCode)
	}
	if s, _ := vip.LoadSettings(context.Background(), st); s.ExternalPool != "" {
		t.Errorf("not cleared: %+v", s)
	}
}

// Nodes configured with --external-vip-pool show that range until the cluster setting replaces it.
func TestNetworkSettingShowsTheNodeFallback(t *testing.T) {
	srv, pw, _ := newTestServerWith(t, func(s *Server) { s.VIPPoolFallback = "192.168.1.50-192.168.1.60" })
	c, _ := loggedInClient(t, srv, pw)
	_, body := get(t, c, srv.URL+"/cluster/settings")
	if !strings.Contains(body, "192.168.1.50-192.168.1.60") || strings.Contains(body, "inside the cluster only") {
		t.Error("settings page does not show the node's configured range")
	}
}

func TestDeployFormSaysWhenBlocksAreClusterOnly(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNodeAt(t, st, "n1", "192.168.1.10")
	c, _ := loggedInClient(t, srv, pw)
	_, body := get(t, c, srv.URL+"/deploy/web/nginx")
	if !strings.Contains(body, "inside the cluster only") || !strings.Contains(body, `href="/cluster/settings#network"`) {
		t.Error("deploy form does not warn that no LAN range is set")
	}
	if err := vip.SaveSettings(context.Background(), st, vip.Settings{ExternalPool: "192.168.1.100"}); err != nil {
		t.Fatal(err)
	}
	if _, body = get(t, c, srv.URL+"/deploy/web/nginx"); strings.Contains(body, "inside the cluster only") {
		t.Error("deploy form still warns with a LAN range set")
	}
}

func TestBlockDetailShowsItsAddress(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "")
	c, csrf := loggedInClient(t, srv, pw)
	deployNginx(t, c, srv.URL, csrf)
	ctx := context.Background()

	if _, body := get(t, c, srv.URL+"/blocks/default/site"); !strings.Contains(body, "Not assigned yet") {
		t.Error("block detail should say the address is pending before the agent assigns one")
	}
	if _, _, err := vip.Assign(ctx, st, "default/site", nil); err != nil {
		t.Fatal(err)
	}
	if _, body := get(t, c, srv.URL+"/blocks/default/site"); !strings.Contains(body, "10.43.0.1") || !strings.Contains(body, "inside the cluster only") {
		t.Error("block detail lacks its internal address")
	}
	if _, _, err := vip.Assign(ctx, st, "default/site", []netip.Prefix{netip.MustParsePrefix("192.168.1.100/24")}); err != nil {
		t.Fatal(err)
	}
	if _, body := get(t, c, srv.URL+"/blocks/default/site"); !strings.Contains(body, "192.168.1.100") || strings.Contains(body, "inside the cluster only") {
		t.Error("block detail lacks its LAN address")
	}
}

// An address assigned while the page is open appears without a reload.
func TestBlockAddressUpdatesLive(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "")
	c, csrf := loggedInClient(t, srv, pw)
	deployNginx(t, c, srv.URL, csrf)
	if _, body := get(t, c, srv.URL+"/blocks/default/site"); !strings.Contains(body, `sse-connect="/blocks/default/site/network/events"`) {
		t.Error("the address card is not in a live region")
	}
	sse := openSSE(t, c, srv.URL+"/blocks/default/site/network/events")
	sse.expect("Not assigned yet", 2*time.Second)
	if _, _, err := vip.Assign(context.Background(), st, "default/site", nil); err != nil {
		t.Fatal(err)
	}
	sse.expect("10.43.0.1", 2*time.Second)
}
