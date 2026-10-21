package web

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

// runFragments streams until changes closes, returning the body and render count.
func runFragments(t *testing.T, changes chan struct{}, tick time.Duration, render func(int) string) (string, int) {
	t.Helper()
	var calls atomic.Int32
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x/events", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		streamFragments(rec, req, "x", changes, tick, func(context.Context) (string, error) {
			return render(int(calls.Add(1))), nil
		})
	}()
	<-done
	return rec.Body.String(), int(calls.Load())
}

func TestStreamFragmentsSendsOnlyChanges(t *testing.T) {
	changes := make(chan struct{})
	go func() {
		for range 3 {
			changes <- struct{}{}
			time.Sleep(300 * time.Millisecond) // past the coalesce window
		}
		close(changes)
	}()
	body, _ := runFragments(t, changes, 0, func(n int) string {
		if n < 3 {
			return "same"
		}
		return "new"
	})
	if got := strings.Count(body, "event: x"); got != 2 {
		t.Errorf("sent %d events, want 2 (the first and the change): %q", got, body)
	}
}

func TestStreamFragmentsSkipsEmptyRender(t *testing.T) {
	changes := make(chan struct{})
	close(changes)
	body, _ := runFragments(t, changes, 0, func(int) string { return "" })
	if strings.Contains(body, "event:") {
		t.Errorf("sent an event for a skipped render: %q", body)
	}
}

func TestStreamFragmentsCoalescesBursts(t *testing.T) {
	changes := make(chan struct{})
	go func() {
		for range 20 {
			changes <- struct{}{}
		}
		close(changes)
	}()
	_, calls := runFragments(t, changes, 0, func(n int) string { return "v" })
	if calls > 3 {
		t.Errorf("rendered %d times for one burst of 20 writes, want at most 3", calls)
	}
}

func TestStreamFragmentsRendersOnTick(t *testing.T) {
	changes := make(chan struct{})
	go func() {
		time.Sleep(200 * time.Millisecond)
		close(changes)
	}()
	body, _ := runFragments(t, changes, 20*time.Millisecond, func(n int) string { return string(rune('a' + n%26)) })
	if got := strings.Count(body, "event: x"); got < 3 {
		t.Errorf("sent %d events over ~10 ticks of changing content, want several", got)
	}
}

// Live lists must stay bounded, and the log tail, which the server replays on
// each reconnect, must clear first; app.js acts on these attributes.
func TestLiveListsAreBoundedAndLogsClearOnReconnect(t *testing.T) {
	cases := map[string][]string{
		"templates/block_logs.html": {`id="log-lines"`, "data-clear-on-open", `data-live-limit="2000"`},
		"templates/events.html":     {`id="event-rows"`, `data-live-limit="500"`},
		"static/app.js":             {"data-clear-on-open", "data-live-limit"},
	}
	for path, wants := range cases {
		fsys := templateFS
		if strings.HasPrefix(path, "static/") {
			fsys = staticFS
		}
		b, err := fsys.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range wants {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s missing %q", path, want)
			}
		}
	}
}

// A failed first load still opens the stream, so the page recovers on its own.
func TestErrorPagesStillStreamAndRecover(t *testing.T) {
	tmpl, err := loadTemplates(templateFS, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		page, connect, container string
		data                     any
	}{
		{"cluster_overview.html", `sse-connect="/cluster/events"`, `id="cluster-fragment"`, clusterOverviewData{Error: "boom"}},
		{"nodes_list.html", `sse-connect="/nodes/events"`, `id="nodes-fragment"`, nodesListData{Error: "boom"}},
		{"health_overview.html", `sse-connect="/health/events"`, `id="health-fragment"`, healthOverviewData{Error: "boom"}},
	}
	for _, tc := range cases {
		var buf strings.Builder
		if err := tmpl.execute(&buf, tc.page, tc.data); err != nil {
			t.Fatalf("%s: %v", tc.page, err)
		}
		body := buf.String()
		i, j := strings.Index(body, tc.container), strings.Index(body, "boom")
		if !strings.Contains(body, tc.connect) || i < 0 || j < i {
			t.Errorf("%s: the error must sit inside the live %s so an update replaces it", tc.page, tc.container)
		}
	}
}

// short feeds the avatar initial; a byte cut would split a non-ASCII letter.
func TestShortKeepsWholeCharacters(t *testing.T) {
	if got := short("Élodie", 1); got != "É" {
		t.Errorf("short = %q, want É", got)
	}
	if got := short("ab", 5); got != "ab" {
		t.Errorf("short = %q, want ab", got)
	}
}

// noLeaderStore fails linearizable revision reads, as a raftstore does without quorum.
type noLeaderStore struct{ store.Store }

func (s noLeaderStore) Revision(ctx context.Context) (store.Revision, error) {
	if !store.StaleFrom(ctx) {
		return 0, context.DeadlineExceeded
	}
	return s.Store.Revision(ctx)
}

// Live pages must keep streaming when the cluster has lost its leader.
func TestFanInWatchesWorksWithoutALeader(t *testing.T) {
	st, err := boltstore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("boltstore.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := fanInWatches(ctx, noLeaderStore{st}, "/a/"); err != nil {
		t.Fatalf("fanInWatches without a leader: %v", err)
	}
}
