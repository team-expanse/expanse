package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/cluster/generation"
	"github.com/expanse/expanse/internal/store"
)

// importGeneration commits a new generation whose desired state is the
// given key set, through the same package RollbackGeneration uses.
func importGeneration(t *testing.T, st store.Store, desc string, keys map[string]string) uint64 {
	t.Helper()
	m := map[store.Key][]byte{}
	for k, v := range keys {
		m[store.Key(k)] = []byte(v)
	}
	n, err := generation.Import(context.Background(), st, generation.EncodeSnapshot(m), "test", desc)
	if err != nil {
		t.Fatalf("generation.Import: %v", err)
	}
	return n
}

func TestGenerationsListShowsHistoryNewestFirstWithCurrentMarked(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	importGeneration(t, st, "deploy web", map[string]string{"/networks/a": "1"})

	resp, body := getPage(t, client, srv, "/generations")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// The generation package stamps CreatedBy "system" and keeps no
	// description yet, so the list shows number, time, parent and hash.
	for _, want := range []string{`href="/generations/2"`, `href="/generations/1"`, "Current", `href="/generations/diff?a=1&b=2"`, "system"} {
		if !strings.Contains(body, want) {
			t.Errorf("generations list missing %q: %s", want, body)
		}
	}
	if strings.Index(body, `href="/generations/2"`) > strings.Index(body, `href="/generations/1"`) {
		t.Error("generations not listed newest first")
	}
}

func TestGenerationDetailListsKeys(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	importGeneration(t, st, "deploy web", map[string]string{"/networks/web": "1", "/networks/db": "2"})

	resp, body := getPage(t, client, srv, "/generations/2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"/networks/web", "/networks/db", "Generation 2"} {
		if !strings.Contains(body, want) {
			t.Errorf("generation detail missing %q", want)
		}
	}
	resp, _ = getPage(t, client, srv, "/generations/99")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown generation status = %d, want 404", resp.StatusCode)
	}
}

func TestGenerationsDiffShowsAddedRemovedChanged(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	importGeneration(t, st, "two", map[string]string{"/networks/keep": "1", "/networks/gone": "1"})
	importGeneration(t, st, "three", map[string]string{"/networks/keep": "2", "/networks/new": "1"})

	resp, body := getPage(t, client, srv, "/generations/diff?a=2&b=3")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"/networks/new", "/networks/gone", "/networks/keep", "Added", "Removed", "Changed"} {
		if !strings.Contains(body, want) {
			t.Errorf("diff missing %q", want)
		}
	}
	resp, _ = getPage(t, client, srv, "/generations/diff?a=x&b=3")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad diff params status = %d, want 400", resp.StatusCode)
	}
}

func TestGenerationRollbackNeedsCSRFAndCreatesANewGeneration(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, csrf := loggedInClient(t, srv, pw)
	importGeneration(t, st, "two", map[string]string{"/networks/x": "1"})

	resp, err := client.Post(srv.URL+"/generations/1/rollback", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("rollback without CSRF status = %d, want 403", resp.StatusCode)
	}

	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp = postVolumeForm(t, client, csrf, srv.URL+"/generations/1/rollback", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/generations" {
		t.Fatalf("rollback status/location = %d %q, want 303 /generations", resp.StatusCode, resp.Header.Get("Location"))
	}
	cur, err := generation.Current(context.Background(), st)
	if err != nil || cur != 3 {
		t.Errorf("current generation after rollback = %d (%v), want 3", cur, err)
	}

	// A later (or nonexistent) target is refused by the generation package as invalid.
	resp = postVolumeForm(t, client, csrf, srv.URL+"/generations/99/rollback", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("rollback to a later generation status = %d, want 400", resp.StatusCode)
	}
}
