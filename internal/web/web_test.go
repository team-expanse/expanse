package web

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/blocks/catalog"
	"github.com/expanse/expanse/internal/blocks/service"
	"github.com/expanse/expanse/internal/blocks/validate"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
	"github.com/expanse/expanse/internal/web/auth"
	pb "github.com/expanse/expanse/proto"
)

// newTestServer returns a running httptest.Server backed by a real
// (on-disk, single-node) store, plus the admin password EnsureAdmin
// generated for it.
func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("boltstore.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	pw, err := auth.EnsureAdmin(context.Background(), st)
	if err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}

	blocks, cat := testBlocksServer(t, st)
	// cluster is nil here: these tests exercise session/auth/block
	// plumbing over a plain boltstore, not cluster status (cluster_test.go
	// covers that against a real single-node raftstore).
	s, err := New("node-1", st, blocks, cat, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.mux)
	t.Cleanup(srv.Close)
	return srv, pw
}

// testBlocksServer builds a real (not mocked) BlockService/CatalogService
// pair over st, loaded from the project's own shipped catalog — the same
// one production loads, so a deploy form built against it (block_test.go)
// exercises real admission, not a stand-in.
func testBlocksServer(t *testing.T, st store.Store) (pb.BlockServiceServer, pb.CatalogServiceServer) {
	t.Helper()
	cat, err := catalog.Load("../../nix/blocks")
	if err != nil {
		t.Fatalf("catalog.Load: %v", err)
	}
	blk := service.New(st, func() validate.Context {
		return validate.Context{Catalog: cat, NodeCount: 1}
	})
	return blk, service.NewCatalogServer(cat)
}

// loggedInClient returns an http.Client with a cookie jar already holding
// a valid session for srv, plus the CSRF token to echo on mutating
// requests.
func loggedInClient(t *testing.T, srv *httptest.Server, password string) (*http.Client, string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := &http.Client{Jar: jar}

	resp, err := client.PostForm(srv.URL+"/login", map[string][]string{
		"username": {auth.AdminUsername},
		"password": {password},
	})
	if err != nil {
		t.Fatalf("login POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200 (after following the post-login redirect)", resp.StatusCode)
	}

	srvURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse %q: %v", srv.URL, err)
	}
	for _, c := range jar.Cookies(srvURL) {
		if c.Name == csrfCookie {
			return client, c.Value
		}
	}
	t.Fatal("login did not set the CSRF cookie")
	return nil, ""
}

func TestUnauthenticatedRequestRedirectsToLogin(t *testing.T) {
	srv, _ := newTestServer(t)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (redirect to /login)", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
}

func TestUnknownPathUnauthenticatedRedirectsToLogin(t *testing.T) {
	srv, _ := newTestServer(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(srv.URL + "/does-not-exist")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 (an unauthenticated request never reveals routing)", resp.StatusCode)
	}
}

func TestWrongPasswordRejected(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.PostForm(srv.URL+"/login", map[string][]string{
		"username": {auth.AdminUsername},
		"password": {"not-the-password"},
	})
	if err != nil {
		t.Fatalf("login POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(strings.ToLower(string(body)), "invalid") {
		t.Errorf("login failure page missing an error message: %s", body)
	}
}

func TestCorrectPasswordAuthorizesFollowUpRequest(t *testing.T) {
	srv, pw := newTestServer(t)
	client, _ := loggedInClient(t, srv, pw)

	resp, err := client.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "node-1") {
		t.Errorf("body missing node ID: %s", body)
	}
}

func TestUnknownPathAuthenticatedIs404(t *testing.T) {
	srv, pw := newTestServer(t)
	client, _ := loggedInClient(t, srv, pw)

	resp, err := client.Get(srv.URL + "/does-not-exist")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	srv, pw := newTestServer(t)
	client, csrf := loggedInClient(t, srv, pw)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/logout", nil)
	req.Header.Set(csrfHeader, csrf)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("logout POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout status = %d, want 303", resp.StatusCode)
	}

	client.CheckRedirect = nil
	resp2, err := client.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET / after logout: %v", err)
	}
	defer resp2.Body.Close()
	if req2 := resp2.Request; req2 == nil || !strings.HasSuffix(req2.URL.Path, "/login") {
		t.Errorf("request after logout ended up at %v, want /login", req2)
	}
}

func TestMutatingRequestWithoutCSRFHeaderRejected(t *testing.T) {
	srv, pw := newTestServer(t)
	client, _ := loggedInClient(t, srv, pw)

	resp, err := client.Post(srv.URL+"/logout", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("POST /logout without CSRF header: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (missing CSRF header)", resp.StatusCode)
	}
}

func TestMutatingRequestWithWrongCSRFTokenRejected(t *testing.T) {
	srv, pw := newTestServer(t)
	client, _ := loggedInClient(t, srv, pw)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/logout", nil)
	req.Header.Set(csrfHeader, "wrong-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /logout with wrong CSRF header: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (mismatched CSRF token)", resp.StatusCode)
	}
}

func TestStaticAssetsServedWithoutAuth(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, path := range []string{"/static/htmx.min.js", "/static/htmx-sse.min.js", "/static/style.css"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, resp.StatusCode)
		}
	}
}
