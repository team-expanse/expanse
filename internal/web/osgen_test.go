package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/osgen"
	"github.com/expanse/expanse/internal/store"
)

func reportGenerations(t *testing.T, st store.Store, node string, rep osgen.Report) {
	t.Helper()
	b, _ := json.Marshal(rep)
	if _, err := st.Put(context.Background(), osgen.ReportKey(node), b); err != nil {
		t.Fatal(err)
	}
}

var testGenerations = []osgen.Generation{
	{Number: 9, Date: time.Now().Add(-time.Hour), Release: "1.14.0", Recorded: true, Current: true},
	{Number: 8, Date: time.Now().Add(-48 * time.Hour), Release: "1.14.0", Exact: true, Recorded: true, Booted: true},
	{Number: 7, Date: time.Now().Add(-96 * time.Hour), Release: "1.13.1"},
}

func TestNodeGenerationsTabShowsReleasesAndDrift(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	c, _ := loggedInClient(t, srv, pw)
	reportGenerations(t, st, "n1", osgen.Report{Generations: testGenerations})

	_, body := get(t, c, srv.URL+"/nodes/n1?tab=generations")
	for _, want := range []string{`aria-current="page">Generations`, "1.14.0, modified", "Release build", "Running", "Booted", "1.13.1", `name="keep" value="3"`} {
		if !strings.Contains(body, want) {
			t.Errorf("generations tab lacks %q", want)
		}
	}
	_, body = get(t, c, srv.URL+"/nodes/n1")
	if !strings.Contains(body, "1.14.0, modified") {
		t.Error("the node header does not show the running system's drift")
	}
}

func TestNodeGenerationsCleanStoresARequest(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	c, csrf := loggedInClient(t, srv, pw)
	reportGenerations(t, st, "n1", osgen.Report{Generations: testGenerations})

	resp, body := postForm(t, c, srv.URL+"/nodes/n1/generations", csrf, url.Values{"keep": {"0"}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "Keep 1 to") {
		t.Errorf("keep 0: status %d, want 400 with the range", resp.StatusCode)
	}
	if _, err := st.Get(context.Background(), osgen.RequestKey("n1")); err == nil {
		t.Fatal("a rejected request was stored")
	}

	resp, body = postForm(t, c, srv.URL+"/nodes/n1/generations", csrf, url.Values{"keep": {"2"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("clean: status %d:\n%s", resp.StatusCode, alerts(body))
	}
	if loc := resp.Header.Get("Location"); loc != "/nodes/n1?tab=generations" {
		t.Errorf("redirect to %q", loc)
	}
	e, err := st.Get(context.Background(), osgen.RequestKey("n1"))
	if err != nil {
		t.Fatal(err)
	}
	var req osgen.CleanRequest
	if json.Unmarshal(e.Value, &req) != nil || req.Keep != 2 {
		t.Fatalf("stored %s", e.Value)
	}
	_, body = get(t, c, srv.URL+"/nodes/n1?tab=generations")
	if !strings.Contains(body, "Cleaning up") {
		t.Error("a pending cleanup is not shown")
	}
}

func TestNodeGenerationsShowsTheLastCleanup(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	c, _ := loggedInClient(t, srv, pw)
	reportGenerations(t, st, "n1", osgen.Report{
		Generations: testGenerations[:2],
		Clean:       &osgen.CleanResult{At: time.Now(), Keep: 2, Removed: []int{7, 6}},
	})
	_, body := get(t, c, srv.URL+"/nodes/n1?tab=generations")
	if !strings.Contains(body, "Removed 2 generations") {
		t.Error("the last cleanup's result is not shown")
	}
}
