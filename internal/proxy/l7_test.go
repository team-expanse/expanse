package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeTableSource is a fixed-snapshot TableSource for L7 tests.
type fakeTableSource struct{ t *Table }

func (f *fakeTableSource) Table() *Table { return f.t }

func svcTable(services ...*Service) *Table {
	t := &Table{Services: map[string]*Service{}}
	for _, s := range services {
		t.Services[s.Key] = s
	}
	return t
}

func TestBuildHTTPRoutesDeterministic(t *testing.T) {
	tbl := svcTable(
		&Service{Key: "default/web", Namespace: "default", Name: "web", Port: 80, TargetPort: 8080},
		&Service{Key: "team/api", Namespace: "team", Name: "api", Port: 80, TargetPort: 9090},
	)
	ht := BuildHTTPRoutes(tbl)
	if len(ht.routes) != 2 {
		t.Fatalf("want 2 routes, got %d: %+v", len(ht.routes), ht.routes)
	}
	// Hosts sorted; each route is <name>.<namespace>.<suffix>.
	if ht.routes[0].Host != "api.team.expanse.internal" || ht.routes[1].Host != "web.default.expanse.internal" {
		t.Fatalf("unexpected route order/hosts: %+v", ht.routes)
	}
	for _, r := range ht.routes {
		if r.PathPrefix != "/" {
			t.Errorf("route %s: want default prefix /, got %q", r.Host, r.PathPrefix)
		}
	}
	if got := ht.routes[1].TargetPort; got != 8080 {
		t.Errorf("web TargetPort = %d, want 8080", got)
	}
}

func TestBuildHTTPRoutesNilAndEmpty(t *testing.T) {
	if ht := BuildHTTPRoutes(nil); len(ht.routes) != 0 {
		t.Fatalf("nil table should yield no routes, got %+v", ht.routes)
	}
	if ht := BuildHTTPRoutes(svcTable()); len(ht.routes) != 0 {
		t.Fatalf("empty table should yield no routes, got %+v", ht.routes)
	}
}

func TestLookupHostExact(t *testing.T) {
	ht := BuildHTTPRoutes(svcTable(
		&Service{Key: "default/web", Namespace: "default", Name: "web", TargetPort: 8080},
		&Service{Key: "team/api", Namespace: "team", Name: "api", TargetPort: 9090},
	))
	cases := []struct {
		host, path, wantKey string
	}{
		{"web.default.expanse.internal", "/", "default/web"},
		{"WEB.DEFAULT.EXPAanse.internal", "/", ""}, // deliberately wrong suffix
		{"web.default.expanse.internal:80", "/", "default/web"},
		{"api.team.expanse.internal", "/v1/x", "team/api"},
		{"unknown.host.example", "/", ""},
	}
	for _, c := range cases {
		got := ht.Lookup(c.host, c.path)
		if c.wantKey == "" {
			if got != nil {
				t.Errorf("Lookup(%q,%q) = %+v, want nil", c.host, c.path, got)
			}
			continue
		}
		if got == nil || got.ServiceKey != c.wantKey {
			t.Errorf("Lookup(%q,%q) = %+v, want service %s", c.host, c.path, got, c.wantKey)
		}
	}
}

func TestLookupLongestPrefix(t *testing.T) {
	// Hand-built table with two same-host routes of different prefix
	// lengths plus a catch-all.
	ht := &HTTPTable{routes: []HTTPRoute{
		{Host: "app.expanse.internal", PathPrefix: "/api/v2", ServiceKey: "api-v2", TargetPort: 8081},
		{Host: "app.expanse.internal", PathPrefix: "/api", ServiceKey: "api", TargetPort: 8080},
		{Host: "app.expanse.internal", PathPrefix: "/", ServiceKey: "app", TargetPort: 8000},
		{Host: "", PathPrefix: "/", ServiceKey: "catchall", TargetPort: 7000},
	}}
	cases := []struct {
		path, wantKey string
	}{
		{"/api/v2/things", "api-v2"},
		{"/api/v2", "api-v2"},
		{"/api/other", "api"},
		{"/everything-else", "app"},
		{"/", "app"},
	}
	for _, c := range cases {
		got := ht.Lookup("app.expanse.internal", c.path)
		if got == nil || got.ServiceKey != c.wantKey {
			t.Errorf("Lookup(app, %q) = %+v, want %s", c.path, got, c.wantKey)
		}
	}
	// No host match anywhere → catch-all.
	got := ht.Lookup("other.example", "/x")
	if got == nil || got.ServiceKey != "catchall" {
		t.Errorf("catch-all: got %+v, want catchall", got)
	}
	// Catch-all prefix shorter than an exact-host route must not
	// shadow it.
	got = ht.Lookup("app.expanse.internal", "/x")
	if got == nil || got.ServiceKey != "app" {
		t.Errorf("host precedence: got %+v, want app", got)
	}
}

func TestLookupPrefixRespectsBoundary(t *testing.T) {
	ht := &HTTPTable{routes: []HTTPRoute{
		{Host: "h.expanse.internal", PathPrefix: "/api", ServiceKey: "api", TargetPort: 80},
	}}
	// "/apiv2" does NOT have "/api" as a path-segment prefix.
	if got := ht.Lookup("h.expanse.internal", "/apiv2"); got != nil {
		t.Errorf("boundary violation: /apiv2 matched %+v", got)
	}
	// "/api" itself matches; "/api/" too.
	for _, p := range []string{"/api", "/api/"} {
		if got := ht.Lookup("h.expanse.internal", p); got == nil {
			t.Errorf("path %q should match /api route", p)
		}
	}
}

func TestL7ServeHTTPRouting(t *testing.T) {
	// Two backends for web, one for api, on httptest servers.
	var hitsWeb, hitsAPI atomic.Int64
	webSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsWeb.Add(1)
		fmt.Fprintf(w, "web:%s", r.URL.Path)
	}))
	defer webSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsAPI.Add(1)
		fmt.Fprint(w, "api")
	}))
	defer apiSrv.Close()

	hostOf := func(srv *httptest.Server) string {
		return strings.TrimPrefix(srv.URL, "http://")
	}
	pool := &fakeTableSource{t: svcTable(
		&Service{
			Key: "default/web", Namespace: "default", Name: "web", TargetPort: 8080,
			Backends: []Backend{{ReplicaIndex: 0, NodeID: "n1", Healthy: true}},
		},
		&Service{
			Key: "team/api", Namespace: "team", Name: "api", TargetPort: 9090,
			Backends: []Backend{{ReplicaIndex: 0, NodeID: "n2", Healthy: true}},
		},
	)}
	l := &L7{
		Pool: pool,
		Resolve: func(_ Backend, target int32) string {
			if target == 9090 {
				return hostOf(apiSrv)
			}
			return hostOf(webSrv)
		},
		Routes: RoutesFrom(pool),
	}
	h := l.Handler()

	// web route
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://web.default.expanse.internal/some/path", nil))
	if rec.Code != 200 || rec.Body.String() != "web:/some/path" {
		t.Errorf("web: code=%d body=%q", rec.Code, rec.Body.String())
	}
	if hitsWeb.Load() != 1 {
		t.Errorf("web backend hits = %d, want 1", hitsWeb.Load())
	}

	// api route
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://api.team.expanse.internal/", nil))
	if rec.Code != 200 || rec.Body.String() != "api" {
		t.Errorf("api: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// unknown host → 404
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://nope.example/", nil))
	if rec.Code != 404 {
		t.Errorf("unknown host: code=%d, want 404", rec.Code)
	}
}

func TestL7ServeHTTPNoHealthyBackends(t *testing.T) {
	pool := &fakeTableSource{t: svcTable(
		&Service{
			Key: "default/web", Namespace: "default", Name: "web", TargetPort: 8080,
			Backends: []Backend{{ReplicaIndex: 0, NodeID: "n1", Healthy: false}},
		},
	)}
	l := &L7{
		Pool:    pool,
		Resolve: func(_ Backend, _ int32) string { return "127.0.0.1:1" },
		Routes:  RoutesFrom(pool),
	}
	rec := httptest.NewRecorder()
	l.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://web.default.expanse.internal/", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code=%d, want 502", rec.Code)
	}
}

func TestL7RoundRobinAcrossBackends(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "b1") }))
	defer srv1.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "b2") }))
	defer srv2.Close()

	pool := &fakeTableSource{t: svcTable(
		&Service{
			Key: "default/web", Namespace: "default", Name: "web", TargetPort: 8080,
			Backends: []Backend{
				{ReplicaIndex: 0, NodeID: "n1", Healthy: true},
				{ReplicaIndex: 1, NodeID: "n2", Healthy: true},
			},
		},
	)}
	l := &L7{
		Pool: pool,
		Resolve: func(b Backend, _ int32) string {
			if b.ReplicaIndex == 0 {
				return strings.TrimPrefix(srv1.URL, "http://")
			}
			return strings.TrimPrefix(srv2.URL, "http://")
		},
		Routes: RoutesFrom(pool),
	}
	h := l.Handler()
	counts := map[string]int{}
	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://web.default.expanse.internal/", nil))
		if rec.Code != 200 {
			t.Fatalf("req %d: code=%d", i, rec.Code)
		}
		counts[rec.Body.String()]++
	}
	if counts["b1"] != 5 || counts["b2"] != 5 {
		t.Errorf("round-robin split = %v, want {b1:5 b2:5}", counts)
	}
}

var _ TableSource = (*fakeTableSource)(nil)

func TestBuildHTTPRoutesDeclared(t *testing.T) {
	tbl := svcTable(
		&Service{
			Key: "default/a", Namespace: "default", Name: "a", TargetPort: 8080,
			HTTPRoutes: []HTTPRouteDecl{
				{Host: "a.test.local", PathPrefix: "/"},
				{Host: "b.test.local", PathPrefix: "/", Service: "b"},
				{Host: "b.test.local", PathPrefix: "/api", Service: "b"},
			},
			Backends: []Backend{{ReplicaIndex: 0, NodeID: "n1", Healthy: true}},
		},
		&Service{
			Key: "default/b", Namespace: "default", Name: "b", TargetPort: 9090,
			Backends: []Backend{{ReplicaIndex: 0, NodeID: "n2", Healthy: true}},
		},
		&Service{Key: "default/c", Namespace: "default", Name: "c", TargetPort: 7070},
	)
	ht := BuildHTTPRoutes(tbl)
	// Declared service (a): exactly its three routes, cross-service
	// targeting resolves to default/b with b's target port.
	if got := ht.Lookup("a.test.local", "/x"); got == nil || got.ServiceKey != "default/a" || got.TargetPort != 8080 {
		t.Errorf("a.test.local: %+v", got)
	}
	if got := ht.Lookup("b.test.local", "/api/x"); got == nil || got.ServiceKey != "default/b" || got.TargetPort != 9090 {
		t.Errorf("b.test.local/api: %+v", got)
	}
	if got := ht.Lookup("b.test.local", "/"); got == nil || got.ServiceKey != "default/b" || got.TargetPort != 9090 {
		t.Errorf("b.test.local/: %+v", got)
	}
	// A declared service must NOT also emit its default host route.
	if got := ht.Lookup("a.default.expanse.internal", "/"); got != nil {
		t.Errorf("declared service leaked default route: %+v", got)
	}
	// Undeclared service still gets the default host route.
	if got := ht.Lookup("c.default.expanse.internal", "/"); got == nil || got.ServiceKey != "default/c" {
		t.Errorf("c default route: %+v", got)
	}
}
