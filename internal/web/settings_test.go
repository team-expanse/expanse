package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/web/auth"
)

func TestAccountSettingsHoldOnlyThePassword(t *testing.T) {
	srv, pw, _ := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	_, body := getPage(t, client, srv, "/settings")
	for _, want := range []string{"Change password", `name="current"`, `name="password"`, `name="confirm"`} {
		if !strings.Contains(body, want) {
			t.Errorf("settings missing %q", want)
		}
	}
	for _, gone := range []string{`name="externalVIPPool"`, "Single sign-on", control.UICAFile} {
		if strings.Contains(body, gone) {
			t.Errorf("account settings still hold cluster-wide %q", gone)
		}
	}
}

func TestClusterSettingsShowOIDCStatusAndCA(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)

	resp, body := getPage(t, client, srv, "/cluster/settings")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, `class="nav-item" href="/cluster" aria-current="page"`) || !strings.Contains(body, `href="/cluster/settings" aria-current="page"`) {
		t.Error("cluster settings not shown as the Cluster section's Settings tab")
	}
	for _, want := range []string{"Not configured", "expanse ctl oidc configure", control.UICAFile} {
		if !strings.Contains(body, want) {
			t.Errorf("settings missing %q", want)
		}
	}
	if strings.Contains(body, `href="/cluster/ui-ca.pem"`) {
		t.Error("download link offered before the CA exists in the store")
	}

	writeOIDCConfig(t, st, "https://idp.example", srv.URL+"/login/oidc/callback", []string{"alice@example.com"})
	rec, _ := json.Marshal(control.CAEntry{CertPEM: []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"), SealedKey: []byte("SEALEDPRIVATEKEYBYTES")})
	if _, err := st.Put(context.Background(), store.Key(control.UICAKey), rec); err != nil {
		t.Fatal(err)
	}
	_, body = getPage(t, client, srv, "/cluster/settings")
	for _, want := range []string{"https://idp.example", "alice@example.com", "test-client", `href="/cluster/ui-ca.pem"`} {
		if !strings.Contains(body, want) {
			t.Errorf("settings after config missing %q", want)
		}
	}
	if strings.Contains(body, "test-client-secret") || strings.Contains(body, "SEALEDPRIVATEKEYBYTES") {
		t.Error("settings page leaks a secret")
	}
}

func TestUICADownloadServesOnlyTheCertificate(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)

	resp, _ := getPage(t, client, srv, "/cluster/ui-ca.pem")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status before CA exists = %d, want 404", resp.StatusCode)
	}
	pem := "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	rec, _ := json.Marshal(control.CAEntry{CertPEM: []byte(pem), SealedKey: []byte("sealed-private-key")})
	if _, err := st.Put(context.Background(), store.Key(control.UICAKey), rec); err != nil {
		t.Fatal(err)
	}
	resp, body := getPage(t, client, srv, "/cluster/ui-ca.pem")
	if resp.StatusCode != http.StatusOK || body != pem {
		t.Errorf("download = %d %q, want 200 and the PEM", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "pem") {
		t.Errorf("Content-Type = %q, want a PEM type", ct)
	}
	if !strings.Contains(resp.Header.Get("Content-Disposition"), "ui-ca.pem") {
		t.Errorf("Content-Disposition = %q, want an attachment filename", resp.Header.Get("Content-Disposition"))
	}

	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := anon.Get(srv.URL + "/cluster/ui-ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusSeeOther {
		t.Errorf("anonymous download status = %d, want 303 to /login", r.StatusCode)
	}
}

func TestChangePasswordValidatesAndThenTakesEffect(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, csrf := loggedInClient(t, srv, pw)

	post := func(current, password, confirm string) (*http.Response, string) {
		resp := postVolumeForm(t, client, csrf, srv.URL+"/settings/password", url.Values{
			"current": {current}, "password": {password}, "confirm": {confirm},
		})
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}

	resp, body := post("wrong", "new-password-123", "new-password-123")
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "current password") {
		t.Errorf("wrong current: status %d, body lacks an inline error: %s", resp.StatusCode, body)
	}
	resp, body = post(pw, "new-password-123", "different")
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "do not match") {
		t.Errorf("mismatch: status %d, body lacks an inline error", resp.StatusCode)
	}
	resp, body = post(pw, "short", "short")
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "at least") {
		t.Errorf("too short: status %d, body lacks an inline error", resp.StatusCode)
	}

	resp, body = post(pw, "new-password-123", "new-password-123")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Password changed") {
		t.Errorf("success: status %d, missing confirmation toast: %s", resp.StatusCode, body)
	}
	if auth.VerifyAdminPassword(context.Background(), st, pw) {
		t.Error("old password still verifies")
	}
	if !auth.VerifyAdminPassword(context.Background(), st, "new-password-123") {
		t.Error("new password does not verify")
	}
}

func TestChangePasswordRequiresCSRF(t *testing.T) {
	srv, pw, _ := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	resp, err := client.PostForm(srv.URL+"/settings/password", url.Values{"current": {pw}, "password": {"x"}, "confirm": {"x"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// Re-checking the current password shares the sign-in cap, so it can't be brute-forced.
func TestChangePasswordSharesTheLoginCap(t *testing.T) {
	old := loginWait
	loginWait = 50 * time.Millisecond
	t.Cleanup(func() { loginWait = old })
	var busy *Server
	srv, pw, _ := newTestServerWith(t, func(s *Server) { busy = s })
	client, csrf := loggedInClient(t, srv, pw)
	for range cap(busy.loginSlots) {
		busy.loginSlots <- struct{}{}
	}
	resp := postVolumeForm(t, client, csrf, srv.URL+"/settings/password", url.Values{
		"current": {"guess"}, "password": {"new-password-123"}, "confirm": {"new-password-123"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status %d with every slot busy, want 429", resp.StatusCode)
	}
}
