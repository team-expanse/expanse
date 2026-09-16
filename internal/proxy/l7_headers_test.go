package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func l7ForTable(tbl *Table, resolve Resolver) *L7 {
	pool := &fakeTableSource{t: tbl}
	return &L7{Pool: pool, Resolve: resolve, Routes: RoutesFrom(pool)}
}

func webTable(backends ...Backend) *Table {
	return svcTable(&Service{
		Key: "default/web", Namespace: "default", Name: "web",
		TargetPort: 8080, Backends: backends,
	})
}

// echoHeaders answers with the forwarded headers as "k=v" lines.
func echoHeaders(t *testing.T, seen *map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*seen)["xff"] = r.Header.Get("X-Forwarded-For")
		(*seen)["xfp"] = r.Header.Get("X-Forwarded-Proto")
		(*seen)["xfh"] = r.Header.Get("X-Forwarded-Host")
		(*seen)["xid"] = r.Header.Get("X-Request-ID")
		fmt.Fprint(w, "ok")
	}))
}

func TestL7HeaderInjection(t *testing.T) {
	seen := map[string]string{}
	srv := echoHeaders(t, &seen)
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	l := l7ForTable(webTable(Backend{ReplicaIndex: 0, NodeID: "n1", Healthy: true}),
		func(_ Backend, _ int32) string { return addr })

	// Fresh client (no XFF, no X-Request-ID).
	req := httptest.NewRequest(http.MethodGet, "http://web.default.expanse.internal/x", nil)
	req.RemoteAddr = "203.0.113.7:4444"
	rec := httptest.NewRecorder()
	l.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if seen["xff"] != "203.0.113.7" {
		t.Errorf("XFF = %q, want client IP", seen["xff"])
	}
	if seen["xfp"] != "http" {
		t.Errorf("XFP = %q, want http (plain inbound)", seen["xfp"])
	}
	if seen["xfh"] != "web.default.expanse.internal" {
		t.Errorf("XFH = %q", seen["xfh"])
	}
	if seen["xid"] == "" {
		t.Errorf("X-Request-ID must be generated when absent")
	}

	// Client-supplied chain + request id: append IP, preserve id.
	req = httptest.NewRequest(http.MethodGet, "http://web.default.expanse.internal/x", nil)
	req.RemoteAddr = "198.51.100.9:5555"
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	req.Header.Set("X-Request-ID", "client-abc")
	rec = httptest.NewRecorder()
	l.Handler().ServeHTTP(rec, req)
	if seen["xff"] != "10.0.0.1, 198.51.100.9" {
		t.Errorf("XFF chain = %q, want appended", seen["xff"])
	}
	if seen["xid"] != "client-abc" {
		t.Errorf("XID = %q, want preserved client id", seen["xid"])
	}
}

// TestL7RetryTable exercises D5.7: retries only for idempotent
// methods, only on connection errors, never on 5xx responses.
func TestL7RetryTable(t *testing.T) {
	closedSrv := newDeadBackend(t)
	dead := closedSrv // addr that refuses connections

	t.Run("GET retries on conn error to healthy backend", func(t *testing.T) {
		var hits atomic.Int64
		good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			fmt.Fprint(w, "good")
		}))
		defer good.Close()
		goodAddr := strings.TrimPrefix(good.URL, "http://")

		l := l7ForTable(webTable(
			Backend{ReplicaIndex: 0, NodeID: "n1", Healthy: true}, // dead
			Backend{ReplicaIndex: 1, NodeID: "n2", Healthy: true}, // good
		), func(b Backend, _ int32) string {
			if b.ReplicaIndex == 0 {
				return dead
			}
			return goodAddr
		})
		// Force start index at the dead backend: Add(1) → 2, 2%2 = 0.
		l.rr.Store(1)
		rec := httptest.NewRecorder()
		l.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://web.default.expanse.internal/", nil))
		if rec.Code != 200 || rec.Body.String() != "good" {
			t.Fatalf("code=%d body=%q, want good", rec.Code, rec.Body.String())
		}
		if hits.Load() != 1 {
			t.Errorf("good backend hits = %d, want 1", hits.Load())
		}
	})

	t.Run("POST does NOT retry on conn error (non-idempotent)", func(t *testing.T) {
		var hits atomic.Int64
		good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
		}))
		defer good.Close()
		l := l7ForTable(webTable(
			Backend{ReplicaIndex: 0, NodeID: "n1", Healthy: true},
			Backend{ReplicaIndex: 1, NodeID: "n2", Healthy: true},
		), func(b Backend, _ int32) string {
			if b.ReplicaIndex == 0 {
				return dead
			}
			return strings.TrimPrefix(good.URL, "http://")
		})
		l.rr.Store(1) // Add(1) → 2, start index 0 = the dead backend
		rec := httptest.NewRecorder()
		l.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://web.default.expanse.internal/", strings.NewReader("x")))
		if rec.Code != http.StatusBadGateway {
			t.Errorf("code=%d, want 502 (no retry)", rec.Code)
		}
		if hits.Load() != 0 {
			t.Errorf("good backend hit %d times, want 0 (must not retry POST)", hits.Load())
		}
	})

	t.Run("5xx is passed through, never retried", func(t *testing.T) {
		var hits atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		l := l7ForTable(webTable(
			Backend{ReplicaIndex: 0, NodeID: "n1", Healthy: true},
			Backend{ReplicaIndex: 1, NodeID: "n2", Healthy: true},
		), func(_ Backend, _ int32) string {
			// Both candidates map to the 500-ing backend; if retries
			// fired on 5xx the hit count would exceed 1.
			return strings.TrimPrefix(srv.URL, "http://")
		})
		rec := httptest.NewRecorder()
		l.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://web.default.expanse.internal/", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("code=%d, want 500 passthrough", rec.Code)
		}
		if n := hits.Load(); n != 1 {
			t.Errorf("backend hits = %d, want exactly 1 (5xx must not retry)", n)
		}
	})
}

// newDeadBackend returns an addr:port that refuses connections.
func newDeadBackend(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // free the port; dial attempts now fail
	return addr
}

// TestL7ResponseHeaderTimeout: a backend that accepts but stalls
// before sending response headers gets cut off at the configured
// ResponseHeaderTimeout.
func TestL7ResponseHeaderTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		fmt.Fprint(w, "too late")
	}))
	defer slow.Close()

	l := l7ForTable(webTable(Backend{ReplicaIndex: 0, NodeID: "n1", Healthy: true}),
		func(_ Backend, _ int32) string { return strings.TrimPrefix(slow.URL, "http://") })
	l.ResponseHeaderTimeout = 50 * time.Millisecond

	start := time.Now()
	rec := httptest.NewRecorder()
	l.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://web.default.expanse.internal/", nil))
	elapsed := time.Since(start)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code=%d, want 502 on header timeout", rec.Code)
	}
	if elapsed > 250*time.Millisecond {
		t.Errorf("timeout took %v, want ~50ms (headers cut off early)", elapsed)
	}
}

// TestL7ConnectTimeout: a dial to a blackholed address is bounded by
// ConnectTimeout, well under the OS default.
func TestL7ConnectTimeout(t *testing.T) {
	// 198.51.100.0/24 is TEST-NET-2; unroutable in the test env.
	blackhole := "198.51.100.254:8080"
	l := l7ForTable(webTable(Backend{ReplicaIndex: 0, NodeID: "n1", Healthy: true}),
		func(_ Backend, _ int32) string { return blackhole })
	l.ConnectTimeout = 100 * time.Millisecond

	start := time.Now()
	rec := httptest.NewRecorder()
	l.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://web.default.expanse.internal/", nil))
	elapsed := time.Since(start)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code=%d, want 502 on connect timeout", rec.Code)
	}
	if elapsed > 2*time.Second {
		t.Errorf("connect took %v, want ~100ms", elapsed)
	}
}

// TestIsConnErr: the retry classifier must accept dial failures and
// reject nil/HTTP-protocol responses-as-errors it cannot classify as
// connection-level.
func TestIsConnErr(t *testing.T) {
	_, port, _ := net.SplitHostPort(newDeadBackend(t))
	connRefused := dialErr(t, "127.0.0.1:"+port)
	if !isConnErr(connRefused) {
		t.Errorf("ECONNREFUSED should classify as conn error")
	}
	if isConnErr(nil) {
		t.Errorf("nil is not a conn error")
	}
	if isConnErr(fmt.Errorf("some app error")) {
		t.Errorf("plain error must not classify as conn error")
	}
}

func dialErr(t *testing.T, addr string) error {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err == nil {
		_ = c.Close()
		t.Fatalf("expected dial to %s to fail", addr)
	}
	return err
}
