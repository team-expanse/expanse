package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/version"
)

// getPage fetches path with the logged-in client and returns the body.
func getPage(t *testing.T, client *http.Client, srv *httptest.Server, path string) (*http.Response, string) {
	t.Helper()
	resp, err := client.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// navLinks is every sidebar destination; every authenticated page must
// render all of them, since the shell is shared, not copied per page.
var navLinks = []string{`href="/"`, `href="/cluster"`, `href="/nodes"`, `href="/blocks"`, `href="/volumes"`, `href="/generations"`, `href="/events"`, `href="/health"`, `href="/settings"`}

func assertShell(t *testing.T, path, body string) {
	t.Helper()
	for _, want := range navLinks {
		if !strings.Contains(body, want) {
			t.Errorf("%s: shell missing nav link %s", path, want)
		}
	}
	if !strings.Contains(body, `class="app-sidebar"`) {
		t.Errorf("%s: missing the sidebar", path)
	}
	if !strings.Contains(body, "Served by") || !strings.Contains(body, version.Get().Version) {
		t.Errorf("%s: footer missing node/version", path)
	}
	if !strings.Contains(body, `/static/app.js`) || !strings.Contains(body, `/static/style.css`) {
		t.Errorf("%s: missing static assets", path)
	}
	if !strings.Contains(body, `<svg class="sprite"`) {
		t.Errorf("%s: missing the inline icon sprite", path)
	}
}

func TestEveryClusterPageRendersTheSharedShell(t *testing.T) {
	srv, pw, _ := newHealthTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	for _, path := range []string{"/", "/cluster", "/nodes", "/nodes/n1", "/volumes", "/volumes/new", "/generations", "/events", "/health", "/settings"} {
		resp, body := getPage(t, client, srv, path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200: %s", path, resp.StatusCode, body)
			continue
		}
		assertShell(t, path, body)
	}
}

func TestBlockPagesRenderTheSharedShell(t *testing.T) {
	srv, pw := newTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	for _, path := range []string{"/blocks", "/blocks/new"} {
		resp, body := getPage(t, client, srv, path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, resp.StatusCode)
			continue
		}
		assertShell(t, path, body)
	}
}

func TestActiveNavItemIsMarked(t *testing.T) {
	srv, pw, _ := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	_, body := getPage(t, client, srv, "/volumes")
	if !strings.Contains(body, `href="/volumes" aria-current="page"`) {
		t.Errorf("/volumes nav item not marked current: %s", body)
	}
	if strings.Contains(body, `href="/cluster" aria-current="page"`) {
		t.Error("/cluster nav item wrongly marked current on /volumes")
	}
}

func TestHeaderShowsClusterNameAndQuorum(t *testing.T) {
	srv, pw, _ := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	_, body := getPage(t, client, srv, "/volumes")
	if !strings.Contains(body, "test-cluster") {
		t.Errorf("header missing the cluster name: %s", body)
	}
	if !strings.Contains(body, "1/1") {
		t.Errorf("header missing quorum: %s", body)
	}
}

func TestStandaloneServerHeaderSaysStandalone(t *testing.T) {
	srv, pw := newTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	_, body := getPage(t, client, srv, "/blocks")
	if !strings.Contains(body, "Standalone") {
		t.Errorf("header on a non-cluster agent should say Standalone: %s", body)
	}
}

func TestPagesCarryAContentSecurityPolicy(t *testing.T) {
	srv, pw := newTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	for _, path := range []string{"/login", "/", "/blocks"} {
		resp, _ := getPage(t, client, srv, path)
		csp := resp.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "script-src 'self'") {
			t.Errorf("GET %s CSP = %q, want a self-only policy", path, csp)
		}
		if strings.Contains(csp, "unsafe-inline") && strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
			t.Errorf("GET %s CSP allows inline script: %q", path, csp)
		}
	}
}

func TestUnknownPathAuthenticatedRendersA404Page(t *testing.T) {
	srv, pw := newTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	resp, body := getPage(t, client, srv, "/no-such-page")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	assertShell(t, "/no-such-page", body)
	if !strings.Contains(body, "not found") {
		t.Errorf("404 page missing its message: %s", body)
	}
}

func TestClusterPagesWithoutClusterServiceAre503(t *testing.T) {
	srv, pw := newTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	for _, path := range []string{"/cluster", "/nodes", "/nodes/node-1", "/generations", "/health"} {
		resp, body := getPage(t, client, srv, path)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("GET %s status = %d, want 503", path, resp.StatusCode)
		}
		assertShell(t, path, body)
	}
}

func TestBlockPagesWithoutBlockServiceAre503(t *testing.T) {
	srv, pw, _ := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	resp, body := getPage(t, client, srv, "/blocks")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	assertShell(t, "/blocks", body)
}

func TestHTMXRequestErrorsArePlainText(t *testing.T) {
	srv, pw, _ := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/blocks", nil)
	req.Header.Set("HX-Request", "true")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if strings.Contains(string(body), "<html") {
		t.Errorf("an HTMX request got a full page instead of a toast-sized message: %s", body)
	}
}

func TestFlashCookieRendersAToastOnceThenClears(t *testing.T) {
	srv, pw, _ := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)

	rec := httptest.NewRecorder()
	setFlash(rec, "success", "Volume data queued for deletion")
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != flashCookie {
		t.Fatalf("setFlash set cookies %+v, want one %s cookie", cookies, flashCookie)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/volumes", nil)
	req.AddCookie(cookies[0])
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Volume data queued for deletion") || !strings.Contains(string(body), `class="toast toast-success"`) {
		t.Errorf("flash not rendered as a toast: %s", body)
	}
	var cleared bool
	for _, c := range resp.Cookies() {
		if c.Name == flashCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("flash cookie not cleared after rendering")
	}
}

func TestLoginPageIsStyledStandalone(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if !strings.Contains(text, `class="login-card"`) || !strings.Contains(text, "/static/style.css") {
		t.Errorf("login page not using the design system: %s", text)
	}
	if strings.Contains(text, `class="app-sidebar"`) {
		t.Error("login page must not render the authenticated shell")
	}
}
