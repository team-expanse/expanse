package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/agent/health"
	"github.com/expanse/expanse/internal/agent/inventory"
	"github.com/expanse/expanse/internal/api"
	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/nodelc"
	"github.com/expanse/expanse/internal/reconcile"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/web/auth"
	pb "github.com/expanse/expanse/proto"
)

// fakeAgent implements api.Agent with a canned Health report and a real
// (never-started) Reconciler, just enough for ListResources/GetHealth to
// exercise their real code paths without running an actual agent loop.
type fakeAgent struct {
	nodeID string
	report *health.Report
	recon  *reconcile.Reconciler
	inv    *inventory.Inventory
}

func (f *fakeAgent) NodeID() string                                         { return f.nodeID }
func (f *fakeAgent) Snapshot() api.Snapshot                                 { return api.Snapshot{Status: "idle"} }
func (f *fakeAgent) Inventory() *inventory.Inventory                        { return f.inv }
func (f *fakeAgent) Health(context.Context) *health.Report                  { return f.report }
func (f *fakeAgent) Reconciler() *reconcile.Reconciler                      { return f.recon }
func (f *fakeAgent) ApplySpec(context.Context, []byte) (int, int, []string) { return 0, 0, nil }
func (f *fakeAgent) DeleteResource(context.Context, string) (bool, error)   { return false, nil }
func (f *fakeAgent) Shutdown(string)                                        {}

// newHealthTestServer wires a real single-node raft cluster (like
// cluster_test.go's newClusterTestServer) behind api.NewServer, plus a
// fakeAgent supplying a canned health report -- so GetHealth/
// ListResources/GetClusterStatus all exercise their real implementations,
// not a mock of this package's own handlers.
func newHealthTestServer(t *testing.T, checks ...health.Result) (*httptest.Server, string, store.Store) {
	t.Helper()
	return newHealthTestServerWithInventory(t, nil, checks...)
}

// newHealthTestServerWithInventory is newHealthTestServer with a canned
// inventory, for the node detail page.
func newHealthTestServerWithInventory(t *testing.T, inv *inventory.Inventory, checks ...health.Result) (*httptest.Server, string, store.Store) {
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

	agent := &fakeAgent{
		nodeID: "n1",
		report: &health.Report{Overall: health.Overall(checks), Checks: checks, At: time.Now()},
		recon:  reconcile.New(res.Store, reconcile.Options{NodeID: "n1"}),
		inv:    inv,
	}
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

func TestUnauthenticatedHealthRedirectsToLogin(t *testing.T) {
	srv, _, _ := newHealthTestServer(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
}

func TestHealthOverviewShowsNodeChecksAndFiresAlert(t *testing.T) {
	srv, pw, _ := newHealthTestServer(t,
		health.Result{Name: "disk-space", Status: health.Healthy},
		health.Result{Name: "memory", Status: health.Unhealthy},
	)
	client, _ := loggedInClient(t, srv, pw)

	resp, err := client.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, want := range []string{"disk-space", "Healthy", "memory", "Unhealthy", "ExpanseNodeUnhealthy"} {
		if !strings.Contains(text, want) {
			t.Errorf("body missing %q: %s", want, text)
		}
	}
}

func TestHealthOverviewShowsQuorum(t *testing.T) {
	srv, pw, _ := newHealthTestServer(t)
	client, _ := loggedInClient(t, srv, pw)

	resp, err := client.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if !strings.Contains(text, "1/1") {
		t.Errorf("body missing quorum 1/1: %s", text)
	}
	if strings.Contains(text, "ExpanseQuorumNoLeader") {
		t.Errorf("single-node cluster has a leader; must not alert ExpanseQuorumNoLeader: %s", text)
	}
}

// TestHealthEventsSSEStreamsLiveUpdate confirms a volume write (no RPC,
// straight to the store) is picked up by the health page's watch, the
// same push-not-poll acceptance shape as cluster_test.go's own SSE test.
func TestHealthEventsSSEStreamsLiveUpdate(t *testing.T) {
	srv, pw, st := newHealthTestServer(t)
	client, _ := loggedInClient(t, srv, pw)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/health/events", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /health/events: %v", err)
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

	drain("event: health", 10*time.Second)

	if err := storage.SaveSpec(context.Background(), st, storage.Spec{ID: "v1", Name: "hvol", SizeBytes: 1024, Replication: 1}); err != nil {
		t.Fatalf("SaveSpec: %v", err)
	}
	if err := storage.SaveStatus(context.Background(), st, "v1", storage.Status{State: storage.StateHealthy}); err != nil {
		t.Fatalf("SaveStatus: %v", err)
	}

	drain("hvol", 10*time.Second)
}

// TestBuildAlertsMirrorsCriticalRules unit-tests the alert derivation
// directly (no HTTP/store plumbing), covering all six critical
// conditions deploy/prometheus/expanse-alerts.rules.yml also fires on.
func TestBuildAlertsMirrorsCriticalRules(t *testing.T) {
	checks := []*pb.CheckResult{
		{Name: "disk-space", Status: pb.Health_HEALTH_HEALTHY},
		{Name: "memory", Status: pb.Health_HEALTH_UNHEALTHY},
	}
	resources := []*pb.Resource{
		{Type: "file", Id: "file:/etc/probe", Health: pb.Health_HEALTH_UNHEALTHY},
	}
	volumes := []*volumeView{
		{Name: "failed-vol", State: storage.StateFailed},
		{Name: "ro-vol", State: storage.StateReadOnly},
		{Name: "ok-vol", State: storage.StateHealthy},
	}
	report := &control.Report{
		Leader: "", Degraded: true,
		Nodes: []control.NodeStatus{{ID: "n3", Lifecycle: nodelc.StateUnreachable}},
	}

	alerts := buildAlerts(checks, resources, volumes, report, (*nodeNames)(nil).name)

	want := map[string]bool{
		"ExpanseNodeUnhealthy":     false,
		"ExpanseResourceUnhealthy": false,
		"ExpanseVolumeFailed":      false,
		"ExpanseVolumeReadOnly":    false,
		"ExpanseQuorumNoLeader":    false,
		"ExpanseQuorumDegraded":    false,
		"ExpanseNodeUnreachable":   false,
	}
	for _, a := range alerts {
		if _, ok := want[a.Name]; !ok {
			t.Errorf("unexpected alert %q", a.Name)
			continue
		}
		want[a.Name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("expected alert %q to fire, got %+v", name, alerts)
		}
	}
	if len(alerts) != len(want) {
		t.Errorf("got %d alerts, want %d: %+v", len(alerts), len(want), alerts)
	}
}

// TestBuildAlertsHealthyClusterFiresNone confirms a fully healthy
// snapshot (the common case) produces zero alerts, not false positives.
func TestBuildAlertsHealthyClusterFiresNone(t *testing.T) {
	checks := []*pb.CheckResult{{Name: "disk-space", Status: pb.Health_HEALTH_HEALTHY}}
	resources := []*pb.Resource{{Type: "file", Id: "file:/etc/probe", Health: pb.Health_HEALTH_HEALTHY}}
	volumes := []*volumeView{{Name: "ok-vol", State: storage.StateHealthy}}
	report := &control.Report{Leader: "n1", Degraded: false}

	if alerts := buildAlerts(checks, resources, volumes, report, (*nodeNames)(nil).name); len(alerts) != 0 {
		t.Errorf("healthy snapshot fired alerts: %+v", alerts)
	}
}

// An unchanged page must not be re-sent on every tick: each swap redraws it.
func TestHealthEventsSkipsUnchangedFragment(t *testing.T) {
	old := healthTick
	healthTick = 20 * time.Millisecond
	t.Cleanup(func() { healthTick = old })

	srv, pw, _ := newHealthTestServer(t, health.Result{Name: "store", Status: health.Healthy})
	client, _ := loggedInClient(t, srv, pw)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/health/events", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /health/events: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body) // ends at the context deadline
	if n := strings.Count(string(body), "event: health"); n != 1 {
		t.Errorf("sent %d health events over many ticks with nothing changed, want 1", n)
	}
}

// Before a cluster forms the node ID is a UUID, so the chrome names the host.
func TestPagesNameTheServingNodeByHostname(t *testing.T) {
	cases := []struct {
		name string
		inv  *inventory.Inventory
		want string
	}{
		{"hostname", &inventory.Inventory{Hostname: "rack1-a"}, "rack1-a"},
		{"no inventory yet", nil, "n1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, pw, _ := newHealthTestServerWithInventory(t, tc.inv)
			client, _ := loggedInClient(t, srv, pw)
			resp, err := client.Get(srv.URL + "/health")
			if err != nil {
				t.Fatalf("GET /health: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			for _, want := range []string{
				"Node <strong>" + tc.want + "</strong>",
				"Served by node <strong>" + tc.want + "</strong>",
				`<h2>Node checks</h2><span class="muted">` + tc.want + "</span>",
			} {
				if !strings.Contains(string(body), want) {
					t.Errorf("body missing %q", want)
				}
			}
		})
	}
}

// The live fragment must carry the same node name as the first render.
func TestHealthEventsNameTheServingNode(t *testing.T) {
	srv, pw, _ := newHealthTestServerWithInventory(t, &inventory.Inventory{Hostname: "rack1-a"})
	client, _ := loggedInClient(t, srv, pw)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/health/events", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /health/events: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if want := `<h2>Node checks</h2><span class="muted">rack1-a</span>`; !strings.Contains(string(body), want) {
		t.Errorf("stream missing %q", want)
	}
}
