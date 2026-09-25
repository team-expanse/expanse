package web

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"

	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
	"github.com/expanse/expanse/internal/web/oidc"
)

// testIdP is a minimal real OIDC provider (discovery + JWKS via
// go-oidc's own oidctest.Server, plus a hand-written /token endpoint)
// standing in for a real IdP like dex -- this exercises web.go's actual
// HTTP wiring (cookies, redirects, session issuance) around the same
// oidc package login_test.go already verifies against the identical
// server shape.
type testIdP struct {
	*httptest.Server
	key   *ecdsa.PrivateKey
	nonce string
	email string
}

func newTestIdP(t *testing.T) *testIdP {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	idp := &testIdP{key: key, email: "alice@example.com"}
	oidcSrv := &oidctest.Server{
		PublicKeys: []oidctest.PublicKey{{PublicKey: key.Public(), KeyID: "test-key", Algorithm: "ES256"}},
		Algorithms: []string{"ES256"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", idp.serveToken)
	mux.Handle("/", oidcSrv)
	idp.Server = httptest.NewServer(mux)
	oidcSrv.SetIssuer(idp.Server.URL)
	t.Cleanup(idp.Server.Close)
	return idp
}

func (idp *testIdP) serveToken(w http.ResponseWriter, r *http.Request) {
	claims := fmt.Sprintf(`{
		"iss": %q, "aud": "test-client", "sub": "alice",
		"exp": %d, "iat": %d,
		"email": %q, "email_verified": true, "nonce": %q
	}`, idp.Server.URL, time.Now().Add(time.Hour).Unix(), time.Now().Unix(), idp.email, idp.nonce)
	idToken := oidctest.SignIDToken(idp.key, "test-key", "ES256", claims)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "test-access-token", "token_type": "Bearer", "id_token": idToken,
	})
}

var clusterSecretForTest = []byte("01234567890123456789012345678901")

// newOIDCTestServer returns a running httptest.Server whose web.Server
// was built with clusterSecretForTest, plus the store backing it (so
// tests can write an OIDC config directly) and the (real) IdP it should
// be configured against.
func newOIDCTestServer(t *testing.T) (*httptest.Server, store.Store, *testIdP) {
	t.Helper()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("boltstore.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	s, err := New("node-1", st, nil, nil, nil, clusterSecretForTest)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.mux)
	t.Cleanup(srv.Close)
	return srv, st, newTestIdP(t)
}

func writeOIDCConfig(t *testing.T, st store.Store, issuer, redirectURL string, allow []string) {
	t.Helper()
	rec, err := oidc.NewConfigRecord(issuer, "test-client", "test-client-secret", redirectURL, allow, clusterSecretForTest)
	if err != nil {
		t.Fatalf("NewConfigRecord: %v", err)
	}
	if _, err := st.Put(context.Background(), store.Key(oidc.ConfigKey), rec); err != nil {
		t.Fatalf("Put OIDC config: %v", err)
	}
}

func TestLoginPageOffersSSOOnlyWhenConfigured(t *testing.T) {
	srv, st, idp := newOIDCTestServer(t)

	resp, err := http.Get(srv.URL + "/login")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if bytes.Contains(body, []byte("/login/oidc/start")) {
		t.Fatal("login page offers SSO before OIDC is configured")
	}

	writeOIDCConfig(t, st, idp.Server.URL, srv.URL+"/login/oidc/callback", []string{"alice@example.com"})

	resp, err = http.Get(srv.URL + "/login")
	if err != nil {
		t.Fatalf("GET /login (after configure): %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte("/login/oidc/start")) {
		t.Fatal("login page does not offer SSO after OIDC was configured")
	}
}

func TestOIDCStartUnconfiguredIs404(t *testing.T) {
	srv, _, _ := newOIDCTestServer(t)

	resp, err := http.Get(srv.URL + "/login/oidc/start")
	if err != nil {
		t.Fatalf("GET /login/oidc/start: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 before OIDC is configured", resp.StatusCode)
	}
}

// oidcClient follows /login/oidc/start's redirect to the (real) IdP,
// which in this test harness answers the authorization endpoint itself
// with an immediate redirect back to our callback (skipping real
// end-user consent -- HandleCallback's own crypto is what's under test,
// identical to login_test.go; this test is about web.go's cookie/
// redirect wiring around it).
func oidcClient(t *testing.T) (*http.Client, *cookiejar.Jar) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	return &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, jar
}

func TestOIDCLoginFullFlow(t *testing.T) {
	srv, st, idp := newOIDCTestServer(t)
	idp.nonce = "" // set below once /start hands us the real nonce via the redirect URL
	writeOIDCConfig(t, st, idp.Server.URL, srv.URL+"/login/oidc/callback", []string{"alice@example.com"})

	client, jar := oidcClient(t)

	// Step 1: /login/oidc/start redirects to the IdP's authorization
	// endpoint and sets the state/nonce cookies.
	resp, err := client.Get(srv.URL + "/login/oidc/start")
	if err != nil {
		t.Fatalf("GET /login/oidc/start: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("start status = %d, want 302", resp.StatusCode)
	}
	authURL, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}
	state := authURL.Query().Get("state")
	idp.nonce = authURL.Query().Get("nonce")
	if state == "" || idp.nonce == "" {
		t.Fatalf("authorization URL missing state/nonce: %s", authURL)
	}

	// Step 2: the browser is redirected straight back to our callback
	// with ?code=...&state=... (standing in for the IdP's own login UI,
	// which this test harness has no use for -- see oidcClient's doc).
	cbURL := srv.URL + "/login/oidc/callback?code=test-code&state=" + state
	resp, err = client.Get(cbURL)
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("callback redirected to %q, want /", loc)
	}

	srvURL, _ := url.Parse(srv.URL)
	var haveSession bool
	for _, c := range jar.Cookies(srvURL) {
		if c.Name == sessionCookie && c.Value != "" {
			haveSession = true
		}
	}
	if !haveSession {
		t.Fatal("callback did not issue a session cookie")
	}
}

func TestOIDCCallbackRejectsForgedState(t *testing.T) {
	srv, st, idp := newOIDCTestServer(t)
	writeOIDCConfig(t, st, idp.Server.URL, srv.URL+"/login/oidc/callback", []string{"alice@example.com"})

	client, _ := oidcClient(t)
	resp, err := client.Get(srv.URL + "/login/oidc/start")
	if err != nil {
		t.Fatalf("GET /login/oidc/start: %v", err)
	}
	resp.Body.Close() // picks up the real state/nonce cookies via the jar

	resp, err = client.Get(srv.URL + "/login/oidc/callback?code=test-code&state=forged-state-value")
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a forged state param", resp.StatusCode)
	}
}

func TestOIDCCallbackRejectsUnauthorizedEmail(t *testing.T) {
	srv, st, idp := newOIDCTestServer(t)
	writeOIDCConfig(t, st, idp.Server.URL, srv.URL+"/login/oidc/callback", []string{"alice@example.com"})
	idp.email = "mallory@example.com"

	client, _ := oidcClient(t)
	resp, err := client.Get(srv.URL + "/login/oidc/start")
	if err != nil {
		t.Fatalf("GET /login/oidc/start: %v", err)
	}
	resp.Body.Close()
	authURL, _ := url.Parse(resp.Header.Get("Location"))
	state := authURL.Query().Get("state")
	idp.nonce = authURL.Query().Get("nonce")

	resp, err = client.Get(srv.URL + "/login/oidc/callback?code=test-code&state=" + state)
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an unauthorized email", resp.StatusCode)
	}
}
