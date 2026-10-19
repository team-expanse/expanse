package web

import (
	"context"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/network/vip"
	"github.com/expanse/expanse/internal/store"
)

func TestRefAddress(t *testing.T) {
	addr := netip.MustParseAddr("10.0.0.50")
	for _, c := range []struct {
		format string
		port   int32
		want   string
	}{
		{"url", 80, "http://10.0.0.50"},
		{"url", 443, "https://10.0.0.50"},
		{"url", 9090, "http://10.0.0.50:9090"},
		{"hostport", 6379, "10.0.0.50:6379"},
	} {
		if got := refAddress(c.format, addr, c.port); got != c.want {
			t.Errorf("%s %d: %q, want %q", c.format, c.port, got, c.want)
		}
	}
}

func TestGrafanaFormOffersAndFillsThePrometheusURL(t *testing.T) {
	srv, pw, st := newTestServerWith(t, withNodes)
	seedNode(t, st, "n1", "")
	c, csrf := loggedInClient(t, srv, pw)
	form := url.Values{"name": {"prom"}, "cpu": {"250m"}, "memory": {"512Mi"}, "storage/tsdb/size": {"1Gi"}}
	if resp, body := postForm(t, c, srv.URL+"/deploy/monitor/prometheus", csrf, form); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("deploy prometheus: %d\n%s", resp.StatusCode, alerts(body))
	}
	p, _, err := vip.Assign(context.Background(), st, "default/prom", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "http://" + p.Addr().String()

	_, body := get(t, c, srv.URL+"/deploy/monitor/grafana")
	for _, s := range []string{`<option value="` + want + `">default/prom</option>`, `value="` + want + `"`} {
		if !strings.Contains(body, s) {
			t.Errorf("grafana form lacks %q", s)
		}
	}
}

func TestRefFieldSaysWhenNoBlockExists(t *testing.T) {
	srv, pw, _ := newTestServerWith(t, func(*Server) {})
	c, _ := loggedInClient(t, srv, pw)
	_, body := get(t, c, srv.URL+"/deploy/monitor/grafana")
	if !strings.Contains(body, `No monitor/prometheus block with an address yet; <a href="/deploy/monitor/prometheus">deploy one</a>`) {
		t.Errorf("no hint to deploy prometheus first")
	}
}

// deployWithAddress deploys a block from form and gives it an address, as the agent would.
func deployWithAddress(t *testing.T, c *http.Client, srv, csrf, typ string, st store.Store, form url.Values) netip.Addr {
	t.Helper()
	if resp, body := postForm(t, c, srv+"/deploy/"+typ, csrf, form); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("deploy %s: %d\n%s", typ, resp.StatusCode, alerts(body))
	}
	p, _, err := vip.Assign(context.Background(), st, "default/"+form.Get("name"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return p.Addr()
}

func TestOptionalRefIsOfferedNotFilled(t *testing.T) {
	srv, pw, st := newTestServerWith(t, withNodes)
	seedNode(t, st, "n1", "")
	c, csrf := loggedInClient(t, srv, pw)
	addr := deployWithAddress(t, c, srv.URL, csrf, "db/redis", st, url.Values{"name": {"r1"}, "cpu": {"100m"}, "memory": {"128Mi"}})

	_, body := get(t, c, srv.URL+"/deploy/db/redis")
	if !strings.Contains(body, `<option value="`+addr.String()+`:6379">default/r1</option>`) {
		t.Error("the redis form does not offer r1 to replicate from")
	}
	if strings.Contains(body, `name="cfg/replicaOf" value="`+addr.String()) {
		t.Error("an optional ref was filled in")
	}
	_, body = get(t, c, srv.URL+"/blocks/default/r1/edit")
	if strings.Contains(body, "default/r1</option>") {
		t.Error("r1's edit page offers r1 itself")
	}
}

// haproxy's frontends and caddy's sites are YAML, so their forms list block addresses to paste in.
func TestProxyFormsListBlockAddressesForTheirUpstreams(t *testing.T) {
	srv, pw, st := newTestServerWith(t, withNodes)
	seedNode(t, st, "n1", "")
	c, csrf := loggedInClient(t, srv, pw)
	addr := deployWithAddress(t, c, srv.URL, csrf, "db/redis", st, url.Values{"name": {"r1"}, "cpu": {"100m"}, "memory": {"128Mi"}})

	for _, typ := range []string{"net/haproxy", "web/caddy"} {
		_, body := get(t, c, srv.URL+"/deploy/"+typ)
		if !strings.Contains(body, "<code>"+addr.String()+":6379</code> default/r1") {
			t.Errorf("the %s form does not list r1's address", typ)
		}
	}
}
