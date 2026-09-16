package proxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync/atomic"
)

// DefaultHostSuffix is the cluster-internal FQDN suffix each block's
// VIP-exposed port is reachable at: "<name>.<namespace>.expanse.internal"
// (§4.3 L7). Explicit per-route Host declarations land with the cert
// manager; until then this convention is the only host class.
const DefaultHostSuffix = "expanse.internal"

// HTTPRoute is one L7 route: requests whose Host matches Host and
// whose path has PathPrefix as a prefix route to the service's
// backends. Host == "" is the catch-all (default) route; PathPrefix
// "" means "/".
type HTTPRoute struct {
	Host       string // lowercase, no port; "" = catch-all
	PathPrefix string // cleaned; "" = "/"
	ServiceKey string // "<namespace>/<name>"
	TargetPort int32
}

// HTTPTable is an immutable snapshot of the L7 routing state, built
// from a Table with the same atomic-swap discipline as T10/T11: the
// builder produces a fresh table, readers hold the old snapshot until
// their request completes. In-flight requests are never confused.
type HTTPTable struct {
	// routes sorted: catch-all routes last, then by host, then by
	// longest PathPrefix first — Lookup relies on this ordering.
	routes []HTTPRoute
}

// BuildHTTPRoutes derives the L7 routes from a pool table snapshot.
// Each VIP-exposed service becomes one default-host route with a "/"
// path prefix (the only host class until Phase 10 adds explicit Host
// declarations). Deterministic order: catch-all last, hosts sorted,
// longer prefixes first.
func BuildHTTPRoutes(t *Table) *HTTPTable {
	tb := &HTTPTable{}
	if t == nil {
		return tb
	}
	keys := make([]string, 0, len(t.Services))
	for k := range t.Services {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		svc := t.Services[k]
		tb.routes = append(tb.routes, HTTPRoute{
			Host:       svc.Name + "." + svc.Namespace + "." + DefaultHostSuffix,
			PathPrefix: "/",
			ServiceKey: svc.Key,
			TargetPort: svc.TargetPort,
		})
	}
	// Catch-all routes (Host == "") sort last; longer prefixes first
	// within the same host so Lookup can return on first match.
	sort.SliceStable(tb.routes, func(i, j int) bool {
		ri, rj := tb.routes[i], tb.routes[j]
		if (ri.Host == "") != (rj.Host == "") {
			return ri.Host != ""
		}
		if ri.Host != rj.Host {
			return ri.Host < rj.Host
		}
		return len(ri.PathPrefix) > len(rj.PathPrefix)
	})
	return tb
}

// Lookup resolves a request's host and path to a route, or nil.
// Matching: routes with an exact Host match beat catch-all routes;
// within that class, the longest matching PathPrefix wins (the table
// is pre-sorted so the first match is the longest).
func (t *HTTPTable) Lookup(host, urlPath string) *HTTPRoute {
	if t == nil {
		return nil
	}
	host = normalizeHost(host)
	clean := cleanPath(urlPath)
	var fallback *HTTPRoute
	for i := range t.routes {
		r := &t.routes[i]
		if r.Host == "" {
			if fallback == nil {
				fallback = r
			}
			continue
		}
		if r.Host != host {
			continue
		}
		if matchPrefix(clean, r.PathPrefix) {
			return r
		}
	}
	return fallback
}

// normalizeHost lowercases and strips any :port suffix and brackets
// from a Host header value.
func normalizeHost(host string) string {
	if h, _, ok := strings.Cut(host, ":"); ok && h != "" && !strings.Contains(h, "]") {
		host = h
	}
	return strings.ToLower(strings.Trim(host, "[]"))
}

// matchPrefix reports whether path has prefix as a segment-boundary
// prefix: "/api" matches "/api" and "/api/x" but not "/apiv2".
func matchPrefix(p, prefix string) bool {
	if prefix == "/" || prefix == "" {
		return true // cleanPath always starts with /
	}
	if !strings.HasPrefix(p, prefix) {
		return false
	}
	rest := p[len(prefix):]
	return rest == "" || rest[0] == '/'
}

// cleanPath normalizes a URL path the way the router compares
// prefixes: ensure a leading slash and no trailing-slash ambiguity
// beyond the prefix itself.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	c := path.Clean("/" + p)
	return c
}

// L7 is one HTTP reverse-proxy instance bound to a VIP:port. It reads
// the pool table per request (fresh snapshot), resolves the route via
// HTTPTable.Lookup, picks a healthy backend round-robin, and proxies
// with httputil.ReverseProxy. Headers, timeouts and idempotent-method
// retries are T14.
type L7 struct {
	Pool    TableSource
	Resolve Resolver // required
	// Routes is the routing snapshot source; defaults to building
	// from Pool. Injections in tests.
	Routes func() *HTTPTable

	rr atomic.Uint64

	server http.Server
	closed atomic.Bool
}

// RoutesFrom returns a Routes func building from the pool table each
// call (per request — same freshness discipline as L4).
func RoutesFrom(p TableSource) func() *HTTPTable {
	return func() *HTTPTable { return BuildHTTPRoutes(p.Table()) }
}

// Handler returns the http.Handler. 404 when no route matches.
func (l *L7) Handler() http.Handler {
	return http.HandlerFunc(l.serveHTTP)
}

func (l *L7) serveHTTP(w http.ResponseWriter, r *http.Request) {
	routes := l.Routes()
	rt := routes.Lookup(r.Host, r.URL.Path)
	if rt == nil {
		http.Error(w, "no route", http.StatusNotFound)
		return
	}
	svc := l.Pool.Table().Service(rt.ServiceKey)
	if svc == nil {
		http.Error(w, "no backends", http.StatusBadGateway)
		return
	}
	cands := svc.Healthy()
	if len(cands) == 0 {
		http.Error(w, "no backends", http.StatusBadGateway)
		return
	}
	// Round-robin over healthy backends (index order rotated by the
	// per-instance counter).
	idx := int(l.rr.Add(1)-1) % len(cands)
	backend := cands[idx]

	target := &url.URL{
		Scheme: "http",
		Host:   l.Resolve(backend, rt.TargetPort),
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = r.Host
		},
		// Transport is T14 (timeouts); http.DefaultTransport stands in
		// for the core card.
	}
	rp.ServeHTTP(w, r)
}
