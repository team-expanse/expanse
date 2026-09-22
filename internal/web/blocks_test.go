package web

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// deployForm returns a POST body encoding manifest as the /blocks form
// expects, the same way url.Values.Encode() would for any HTML form.
func deployForm(manifest string) string {
	return url.Values{"manifest": {manifest}}.Encode()
}

const echoManifest = `apiVersion: expanse.io/v1
kind: Block
metadata:
  name: web
  namespace: default
spec:
  type: util/echo
  replicas: 1
  config:
    port: 18080
    body: "hi\n"
`

func TestUnauthenticatedBlocksRedirectsToLogin(t *testing.T) {
	srv, _ := newTestServer(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(srv.URL + "/blocks")
	if err != nil {
		t.Fatalf("GET /blocks: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 (redirect to /login)", resp.StatusCode)
	}
}

func TestBlockDeployListDetailScaleDelete(t *testing.T) {
	srv, pw := newTestServer(t)
	client, csrf := loggedInClient(t, srv, pw)

	// --- deploy ---
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/blocks", strings.NewReader(deployForm(echoManifest)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(csrfHeader, csrf)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /blocks: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("deploy status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/blocks/default/web" {
		t.Fatalf("Location = %q, want /blocks/default/web", loc)
	}
	client.CheckRedirect = nil

	// --- list ---
	resp, err = client.Get(srv.URL + "/blocks")
	if err != nil {
		t.Fatalf("GET /blocks: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "default/web") {
		t.Errorf("blocks list missing the deployed block: %s", body)
	}

	// --- detail ---
	resp, err = client.Get(srv.URL + "/blocks/default/web")
	if err != nil {
		t.Fatalf("GET /blocks/default/web: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "util/echo") {
		t.Errorf("block detail missing its type: %s", body)
	}

	// --- scale ---
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/blocks/default/web/scale", strings.NewReader("replicas=2"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(csrfHeader, csrf)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST scale: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scale status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "/2") {
		t.Errorf("scaled fragment missing the new replica count: %s", body)
	}

	// --- delete ---
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/blocks/default/web/delete", nil)
	req.Header.Set(csrfHeader, csrf)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete status = %d, want 303", resp.StatusCode)
	}
	client.CheckRedirect = nil

	resp, err = client.Get(srv.URL + "/blocks/default/web")
	if err != nil {
		t.Fatalf("GET /blocks/default/web after delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status after delete = %d, want 404", resp.StatusCode)
	}
}

func TestBlockDeployInvalidManifestRejected(t *testing.T) {
	srv, pw := newTestServer(t)
	client, csrf := loggedInClient(t, srv, pw)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/blocks", strings.NewReader(deployForm("not: valid: yaml: :::")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(csrfHeader, csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /blocks: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", resp.StatusCode, body)
	}
}

// TestBlockEventsSSEStreamsInitialSnapshot: X5's live-status requirement
// -- a client connecting to /events sees the block's current state
// without any write happening first (the pre-any-event snapshot).
func TestBlockEventsSSEStreamsInitialSnapshot(t *testing.T) {
	srv, pw := newTestServer(t)
	client, csrf := loggedInClient(t, srv, pw)

	deployReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/blocks", strings.NewReader(deployForm(echoManifest)))
	deployReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	deployReq.Header.Set(csrfHeader, csrf)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(deployReq)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	resp.Body.Close()
	client.CheckRedirect = nil

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/blocks/default/web/events", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	sc := bufio.NewScanner(resp.Body)
	var saw bool
	for sc.Scan() {
		line := sc.Text()
		if line == "event: block" {
			saw = true
		}
		if saw && strings.Contains(line, "util/echo") {
			return // the initial snapshot arrived
		}
	}
	t.Fatalf("SSE stream ended without an initial snapshot event (scan err: %v)", sc.Err())
}
