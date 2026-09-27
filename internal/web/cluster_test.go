package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/agent/health"
	"github.com/expanse/expanse/internal/api"
	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/reconcile"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/web/auth"
)

// newClusterTestServer wires a real single-node raft cluster (control.Init,
// the same bootstrap `expanse cluster init` runs) behind api.NewServer, so
// GetClusterStatus exercises the real code path — not a mock — the same
// way blocks_test.go tests against a real service.Server. The agent is a
// fakeAgent with no checks or inventory, enough for the dashboard's
// health read. The store is returned too, so a test can write directly
// to it (simulating a join) without a second RPC seam.
func newClusterTestServer(t *testing.T) (*httptest.Server, string, store.Store) {
	t.Helper()
	dir := t.TempDir()
	res, err := control.Init(context.Background(), control.InitOptions{
		DataDir: dir, NodeID: "n1", Name: "test-cluster",
		AdvertiseAddr: freeAddr(t), BindAddr: freeAddr(t),
	})
	if err != nil {
		t.Fatalf("control.Init: %v", err)
	}
	t.Cleanup(func() { _ = res.Store.Close() })

	pw, err := auth.EnsureAdmin(context.Background(), res.Store)
	if err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}

	agent := &fakeAgent{nodeID: "n1", report: &health.Report{Overall: health.Healthy, At: time.Now()}, recon: reconcile.New(res.Store, reconcile.Options{NodeID: "n1"})}
	apiSrv := api.NewServer(agent, res.Store, slog.Default())
	s, err := New("n1", res.Store, nil, nil, apiSrv, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startEventLog(t, s)
	srv := httptest.NewServer(s.mux)
	t.Cleanup(srv.Close)
	return srv, pw, res.Store
}

// startEventLog runs the event recorder Serve would start, for tests
// that drive s.mux directly through httptest.
func startEventLog(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.events.run(ctx, s.store)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestUnauthenticatedClusterRedirectsToLogin(t *testing.T) {
	srv, _, _ := newClusterTestServer(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(srv.URL + "/cluster")
	if err != nil {
		t.Fatalf("GET /cluster: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
}

func TestClusterOverviewShowsSingleNodeReport(t *testing.T) {
	srv, pw, _ := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)

	resp, err := client.Get(srv.URL + "/cluster")
	if err != nil {
		t.Fatalf("GET /cluster: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if !strings.Contains(text, "n1") {
		t.Errorf("body missing node ID n1: %s", text)
	}
	if !strings.Contains(text, "1/1") {
		t.Errorf("body missing quorum 1/1: %s", text)
	}
}

// TestClusterEventsSSEStreamsLiveUpdate is B1's own acceptance criterion
// at unit-test scope: connect over SSE, see the single-node snapshot,
// then write a second node record directly (mimicking a join the join
// service would otherwise perform) and confirm a second event reflects
// the new node — a push, not a poll.
func TestClusterEventsSSEStreamsLiveUpdate(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/cluster/events", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /cluster/events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	type read struct {
		text string
		err  error
	}
	lines := make(chan read, 16)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				lines <- read{text: string(buf[:n])}
			}
			if err != nil {
				lines <- read{err: err}
				return
			}
		}
	}()

	var got string
	drain := func(want string, timeout time.Duration) {
		t.Helper()
		deadline := time.After(timeout)
		for !strings.Contains(got, want) {
			select {
			case r := <-lines:
				if r.err != nil {
					t.Fatalf("read SSE stream: %v (got so far: %q)", r.err, got)
				}
				got += r.text
			case <-deadline:
				t.Fatalf("%q not seen within %s (got: %q)", want, timeout, got)
			}
		}
	}

	drain("event: cluster", 10*time.Second)

	// A second node record, written directly to the store: the watch on
	// join.NodesKeyPrefix must pick this up without any RPC round trip.
	rec := join.NodeRecord{ID: "n2", RaftAddr: "127.0.0.1:1", APIAddr: "127.0.0.1:2", Role: "voter"}
	b, _ := json.Marshal(rec)
	if _, err := st.Put(context.Background(), store.Key(join.NodesKeyPrefix+"n2"), b); err != nil {
		t.Fatalf("Put n2: %v", err)
	}

	drain("n2", 10*time.Second)
}
