package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"

	"github.com/expanse/expanse/internal/errors"
)

// testIdP wraps oidctest.Server (a real, spec-following discovery/JWKS
// server from go-oidc's own test suite -- not a hand-rolled stub of the
// library under test) with a minimal /token endpoint of our own, so
// oauth2.Config.Exchange has something real to talk to. Every ID token
// it returns is a genuinely signed, genuinely verified JWT: only the
// authorization step (browser consent) is skipped, since HandleCallback
// never performs it itself either -- that is StartLogin's redirect,
// exercised separately below.
type testIdP struct {
	*httptest.Server
	key   *ecdsa.PrivateKey
	nonce string // embedded in the next /token response's id_token
	email string
	// nextExchangeFails, if set, makes /token answer 400 instead of a
	// token -- exercises HandleCallback's code-exchange failure path.
	nextExchangeFails bool
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
	mux.Handle("/", oidcSrv) // discovery + /keys
	idp.Server = httptest.NewServer(mux)
	oidcSrv.SetIssuer(idp.Server.URL)
	t.Cleanup(idp.Server.Close)
	return idp
}

func (idp *testIdP) serveToken(w http.ResponseWriter, r *http.Request) {
	if idp.nextExchangeFails {
		http.Error(w, "invalid_grant", http.StatusBadRequest)
		return
	}
	claims := fmt.Sprintf(`{
		"iss": %q, "aud": "test-client", "sub": "alice",
		"exp": %d, "iat": %d,
		"email": %q, "email_verified": true, "nonce": %q
	}`, idp.Server.URL, time.Now().Add(time.Hour).Unix(), time.Now().Unix(), idp.email, idp.nonce)
	idToken := oidctest.SignIDToken(idp.key, "test-key", "ES256", claims)
	resp := map[string]any{
		"access_token": "test-access-token",
		"token_type":   "Bearer",
		"id_token":     idToken,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func testConfig(t *testing.T, issuer string) *Config {
	t.Helper()
	rec, err := NewConfigRecord(issuer, "test-client", "test-client-secret", "https://ui.example/login/oidc/callback", []string{"alice@example.com"}, testSecret)
	if err != nil {
		t.Fatalf("NewConfigRecord: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(rec, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &cfg
}

func TestStartLoginBuildsAuthURL(t *testing.T) {
	ctx := context.Background()
	idp := newTestIdP(t)
	cfg := testConfig(t, idp.Server.URL)

	authURL, state, nonce, err := StartLogin(ctx, cfg, testSecret)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if state == "" || nonce == "" || state == nonce {
		t.Fatalf("expected distinct non-empty state/nonce, got %q / %q", state, nonce)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("authURL not a valid URL: %v", err)
	}
	q := u.Query()
	if q.Get("client_id") != "test-client" {
		t.Errorf("client_id = %q, want test-client", q.Get("client_id"))
	}
	if q.Get("state") != state {
		t.Errorf("state param = %q, want %q", q.Get("state"), state)
	}
	if q.Get("nonce") != nonce {
		t.Errorf("nonce param = %q, want %q", q.Get("nonce"), nonce)
	}
	if !strings.Contains(q.Get("scope"), "openid") {
		t.Errorf("scope %q missing openid", q.Get("scope"))
	}

	// Two calls must never reuse state/nonce.
	_, state2, nonce2, err := StartLogin(ctx, cfg, testSecret)
	if err != nil {
		t.Fatalf("StartLogin (2nd): %v", err)
	}
	if state2 == state || nonce2 == nonce {
		t.Fatal("StartLogin reused state or nonce across calls")
	}
}

func TestHandleCallbackSuccess(t *testing.T) {
	ctx := context.Background()
	idp := newTestIdP(t)
	cfg := testConfig(t, idp.Server.URL)
	idp.nonce = "the-nonce"

	userID, err := HandleCallback(ctx, cfg, testSecret, "any-code", "the-nonce")
	if err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	if userID != "alice@example.com" {
		t.Fatalf("userID = %q, want alice@example.com", userID)
	}
}

func TestHandleCallbackNonceMismatch(t *testing.T) {
	ctx := context.Background()
	idp := newTestIdP(t)
	cfg := testConfig(t, idp.Server.URL)
	idp.nonce = "the-real-nonce"

	if _, err := HandleCallback(ctx, cfg, testSecret, "any-code", "an-attacker-supplied-nonce"); errors.KindOf(err) != errors.KindPermission {
		t.Fatalf("expected KindPermission on nonce mismatch, got %v", err)
	}
}

func TestHandleCallbackEmailNotAllowed(t *testing.T) {
	ctx := context.Background()
	idp := newTestIdP(t)
	cfg := testConfig(t, idp.Server.URL)
	idp.nonce = "n1"
	idp.email = "mallory@example.com" // not in cfg.AllowedEmails

	if _, err := HandleCallback(ctx, cfg, testSecret, "any-code", "n1"); errors.KindOf(err) != errors.KindPermission {
		t.Fatalf("expected KindPermission for an unauthorized email, got %v", err)
	}
}

func TestHandleCallbackExchangeFails(t *testing.T) {
	ctx := context.Background()
	idp := newTestIdP(t)
	cfg := testConfig(t, idp.Server.URL)
	idp.nextExchangeFails = true

	if _, err := HandleCallback(ctx, cfg, testSecret, "bad-code", "n1"); errors.KindOf(err) != errors.KindPermission {
		t.Fatalf("expected KindPermission on a failed code exchange, got %v", err)
	}
}

func TestHandleCallbackWrongClientSecretCannotUnseal(t *testing.T) {
	ctx := context.Background()
	idp := newTestIdP(t)
	cfg := testConfig(t, idp.Server.URL)
	idp.nonce = "n1"

	wrongSecret := []byte("wrong-cluster-secret-wrong-cluster")
	if _, err := HandleCallback(ctx, cfg, wrongSecret, "any-code", "n1"); err == nil {
		t.Fatal("expected an error unsealing the client secret with the wrong cluster secret")
	}
}
