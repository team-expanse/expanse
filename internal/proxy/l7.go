package proxy

import (
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	experrors "github.com/expanse/expanse/internal/errors"
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
		if len(svc.HTTPRoutes) > 0 {
			for _, decl := range svc.HTTPRoutes {
				key := svc.Key
				target := int32(0)
				if decl.Service != "" {
					key = svc.Namespace + "/" + decl.Service
					if ts := t.Service(key); ts != nil {
						target = ts.TargetPort
					}
				}
				if target == 0 {
					target = svc.TargetPort
				}
				tb.routes = append(tb.routes, HTTPRoute{
					Host:       decl.Host,
					PathPrefix: cleanPath(decl.PathPrefix),
					ServiceKey: key,
					TargetPort: target,
				})
			}
			continue
		}
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
// HTTPTable.Lookup, and proxies with httputil.ReverseProxy. Backend
// selection, header injection, timeouts and idempotent-method retries
// live in the per-request transport (T14).
type L7 struct {
	Pool    TableSource
	Resolve Resolver // required
	// Routes is the routing snapshot source; defaults to building
	// from Pool. Injections in tests.
	Routes func() *HTTPTable

	// Timeouts (§4.3): connect 5 s, response header 30 s, idle 90 s
	// by default; configurable per L7 instance (per-route configuration
	// lands with explicit route declarations).
	ConnectTimeout        time.Duration // default 5 s
	ResponseHeaderTimeout time.Duration // default 30 s
	IdleTimeout           time.Duration // default 90 s

	// MaxRetries caps retries for idempotent methods (§9 D5.7):
	// GET/HEAD/OPTIONS only, connection errors only, never on 5xx.
	// Default 2.
	MaxRetries int

	// GetCertificate is the TLS/SNI termination hook point, deferred
	// to Phase 10's cert manager. Until then callers wiring TLS should
	// use NoCertYet (returns KindUnavailable); L7 ships plain
	// HTTP/h2c — no self-signed stand-ins (T14 contract).
	GetCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)

	rr        atomic.Uint64
	transport http.RoundTripper
	initOnce  sync.Once
	server    http.Server
	closed    atomic.Bool
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
	l.initOnce.Do(l.initTransport)
	maxRetries := l.MaxRetries
	if maxRetries == 0 {
		maxRetries = DefaultMaxRetries
	}
	rp := &httputil.ReverseProxy{
		// Rewrite only touches per-client state (headers, Host); the
		// backend target is chosen per attempt by l7Transport so
		// idempotent requests can rotate on connection errors.
		Rewrite: func(pr *httputil.ProxyRequest) {
			injectForwardedHeaders(pr, r)
			pr.Out.Host = r.Host
		},
		Transport: &l7Transport{
			inner:      l.transport,
			resolve:    l.Resolve,
			candidates: cands,
			targetPort: rt.TargetPort,
			rr:         &l.rr,
			maxRetries: maxRetries,
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			// Connection-level failures surface as 502, not 500.
			slog.Debug("l7 proxy error", "error", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// DefaultMaxRetries is the §4.3 retry budget for idempotent methods.
const DefaultMaxRetries = 2

// idempotentMethods are the only methods that may be retried (D5.7:
// a retried non-idempotent request can duplicate side effects).
func idempotentMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// l7Transport performs backend selection per attempt: it starts the
// request at the L7-wide round-robin cursor and, for idempotent
// methods only, rotates to the next candidate on CONNECTION errors
// (never after a response — a 5xx is passed through untouched; see
// §9 D5.7). Non-idempotent methods use the first selected backend
// only.
type l7Transport struct {
	inner      http.RoundTripper
	resolve    Resolver
	candidates []Backend
	targetPort int32
	rr         *atomic.Uint64
	maxRetries int
}

func (t *l7Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := int(t.rr.Add(1) % uint64(len(t.candidates)))
	idem := idempotentMethod(req.Method)
	var lastErr error
	for attempt := 0; attempt <= t.maxRetries; attempt++ {
		b := t.candidates[(start+attempt)%len(t.candidates)]
		addr := t.resolve(b, t.targetPort)
		tr := req.Clone(req.Context())
		tr.URL = &url.URL{
			Scheme:   "http",
			Host:     addr,
			Path:     req.URL.Path,
			RawQuery: req.URL.RawQuery,
		}
		resp, err := t.inner.RoundTrip(tr)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !idem || attempt == t.maxRetries || !isConnErr(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

// isConnErr reports whether err is a connection-level failure (dial
// refused/reset/timeout) as opposed to a response the backend sent.
func isConnErr(err error) bool {
	if err == nil {
		return false
	}
	var op *net.OpError
	if errors.As(err, &op) {
		switch op.Op {
		case "dial", "read", "write":
			return true
		}
	}
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.EOF) // server closed conn before response
}

// injectForwardedHeaders sets the §4.3 header set on the outbound
// request: X-Forwarded-For (append the client IP to any existing
// chain), X-Forwarded-Proto (https iff the inbound request arrived
// over TLS), X-Forwarded-Host, and X-Request-ID (generated UUIDv4
// unless the client supplied one).
func injectForwardedHeaders(pr *httputil.ProxyRequest, in *http.Request) {
	clientIP, _, _ := net.SplitHostPort(in.RemoteAddr)
	if clientIP == "" {
		clientIP = in.RemoteAddr
	}
	prior := in.Header.Get("X-Forwarded-For")
	if prior != "" {
		pr.Out.Header.Set("X-Forwarded-For", prior+", "+clientIP)
	} else {
		pr.Out.Header.Set("X-Forwarded-For", clientIP)
	}
	proto := "http"
	if in.TLS != nil {
		proto = "https"
	}
	pr.Out.Header.Set("X-Forwarded-Proto", proto)
	pr.Out.Header.Set("X-Forwarded-Host", in.Host)
	xreq := in.Header.Get("X-Request-ID")
	if xreq == "" {
		xreq = newRequestID()
	}
	pr.Out.Header.Set("X-Request-ID", xreq)
}

// newRequestID returns a random UUIDv4-formatted request id.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Crypto randomness failure is unrecoverable; fall back to a
		// time-unique id rather than failing the request.
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// initTransport builds the shared outbound transport with the §4.3
// timeouts: connect DialContext timeout, response-header timeout, and
// idle-connection timeout. Built once per L7 instance.
func (l *L7) initTransport() {
	connect := l.ConnectTimeout
	if connect == 0 {
		connect = DefaultDialTimeout
	}
	rh := l.ResponseHeaderTimeout
	if rh == 0 {
		rh = DefaultResponseHeaderTimeout
	}
	idle := l.IdleTimeout
	if idle == 0 {
		idle = DefaultIdleTimeout
	}
	l.transport = &http.Transport{
		Proxy:                 nil, // direct to backends, never via env proxy
		DialContext:           (&net.Dialer{Timeout: connect}).DialContext,
		ResponseHeaderTimeout: rh,
		IdleConnTimeout:       idle,
		MaxIdleConnsPerHost:   64,
	}
}

// NoCertYet is the Phase 10 placeholder for L7.GetCertificate: TLS
// termination is explicitly deferred — callers get a KindUnavailable
// error, never a self-signed substitute.
func NoCertYet(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return nil, experrors.New(experrors.KindUnavailable, "l7.GetCertificate",
		"TLS termination arrives with the Phase 10 cert manager")
}
