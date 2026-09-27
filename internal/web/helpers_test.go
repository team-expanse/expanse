package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// sseStream reads an SSE response in the background so a test can wait
// for a substring (a pushed event) with a timeout.
type sseStream struct {
	t     *testing.T
	resp  *http.Response
	mu    sync.Mutex
	buf   strings.Builder
	errCh chan error
}

func openSSE(t *testing.T, client *http.Client, u string) *sseStream {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", u, resp.StatusCode)
	}
	s := &sseStream{t: t, resp: resp, errCh: make(chan error, 1)}
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(b)
			if n > 0 {
				s.mu.Lock()
				s.buf.Write(b[:n])
				s.mu.Unlock()
			}
			if err != nil {
				s.errCh <- err
				return
			}
		}
	}()
	return s
}

func (s *sseStream) got() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// expect blocks until want has been streamed or timeout elapses.
func (s *sseStream) expect(want string, timeout time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for !strings.Contains(s.got(), want) {
		select {
		case err := <-s.errCh:
			s.t.Fatalf("SSE stream ended: %v (got so far: %q)", err, s.got())
		default:
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("%q not seen within %s (got: %q)", want, timeout, s.got())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitFor polls cond until it holds or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// sessionCookieValue returns the logged-in client's session ID, so a
// test can assert it never appears in a rendered page.
func sessionCookieValue(t *testing.T, client *http.Client, srv *httptest.Server) string {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	for _, c := range client.Jar.Cookies(u) {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	t.Fatal("no session cookie in jar")
	return ""
}
