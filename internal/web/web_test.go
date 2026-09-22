package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIndexServesPlaceholderWithNodeID(t *testing.T) {
	s, err := New("node-1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
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
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	s, err := New("node-1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/does-not-exist")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestStaticAssetsServed(t *testing.T) {
	s, err := New("node-1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.mux)
	defer srv.Close()

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
