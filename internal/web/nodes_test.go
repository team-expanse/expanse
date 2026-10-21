package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/cluster/control"

	"github.com/expanse/expanse/internal/agent/health"
	"github.com/expanse/expanse/internal/agent/inventory"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
)

// putTestNode writes the record and status a joined node would have.
func putTestNode(t *testing.T, st store.Store, rec join.NodeRecord, status string) {
	t.Helper()
	b, _ := json.Marshal(rec)
	if _, err := st.Put(context.Background(), store.Key(join.NodesKeyPrefix+rec.ID), b); err != nil {
		t.Fatalf("Put node %s: %v", rec.ID, err)
	}
	if status != "" {
		if _, err := st.Put(context.Background(), store.Key(join.NodesKeyPrefix+rec.ID+"/status"), []byte(status)); err != nil {
			t.Fatalf("Put node status %s: %v", rec.ID, err)
		}
	}
}

func TestNodesListShowsEveryNodeWithRoleAndHealth(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	putTestNode(t, st, join.NodeRecord{ID: "n2", RaftAddr: "10.0.0.2:7447", APIAddr: "10.0.0.2:7443", Role: "voter", State: "unreachable"}, "health=healthy")
	putTestNode(t, st, join.NodeRecord{ID: "n3", RaftAddr: "10.0.0.3:7447", APIAddr: "10.0.0.3:7443", Role: "witness"}, "health=degraded")

	resp, body := getPage(t, client, srv, "/nodes")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{`href="/nodes/n1"`, `href="/nodes/n2"`, `href="/nodes/n3"`, "Leader", "Unreachable", "Witness", "Degraded", "10.0.0.2:7443"} {
		if !strings.Contains(body, want) {
			t.Errorf("nodes list missing %q", want)
		}
	}
}

func TestNodeDetailForServingNodeShowsInventoryChecksAndReconciler(t *testing.T) {
	srv, pw, _ := newHealthTestServerWithInventory(t, &inventory.Inventory{
		NodeID: "n1", Hostname: "rack1-a",
		OS:      inventory.OSInfo{Name: "NixOS", Version: "25.05", Kernel: "6.12.1"},
		CPU:     inventory.CPUInfo{Model: "AMD EPYC 7302P", Cores: 16, Threads: 32},
		Memory:  inventory.MemoryInfo{Total: 64 << 30, Available: 40 << 30},
		Disks:   []inventory.DiskInfo{{Path: "/dev/nvme0n1", Model: "Samsung PM9A3", Size: 960 << 30}},
		Network: []inventory.NICInfo{{Name: "eno1", MAC: "aa:bb:cc:dd:ee:01", SpeedMbps: 10000, Addresses: []string{"10.0.0.1/24"}, Up: true}},
		GPUs:    []inventory.GPUInfo{{Vendor: "NVIDIA", Model: "A2", VRAM: 16384, Driver: "nvidia"}},
	}, health.Result{Name: "disk-space", Status: health.Healthy, Message: "/ 41% used"})
	client, _ := loggedInClient(t, srv, pw)

	resp, body := getPage(t, client, srv, "/nodes/n1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	for _, want := range []string{"rack1-a", "NixOS", "AMD EPYC 7302P", "16 cores", "/dev/nvme0n1", "Samsung PM9A3", "eno1", "aa:bb:cc:dd:ee:01", "NVIDIA", "disk-space", "/ 41% used", "Reconciler", "Idle"} {
		if !strings.Contains(body, want) {
			t.Errorf("node detail missing %q", want)
		}
	}
}

func TestNodeDetailBeforeInventoryIsCollectedSaysSo(t *testing.T) {
	srv, pw, _ := newHealthTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	resp, body := getPage(t, client, srv, "/nodes/n1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "not collected yet") {
		t.Errorf("node detail should explain the missing inventory: %s", body)
	}
}

func TestNodeDetailForAnotherNodeLabelsTheLocalOnlyData(t *testing.T) {
	srv, pw, st := newHealthTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	putTestNode(t, st, join.NodeRecord{ID: "n2", RaftAddr: "10.0.0.2:7447", APIAddr: "10.0.0.2:7443", Role: "voter", JoinedAt: time.Now().UnixNano()}, "health=healthy")

	resp, body := getPage(t, client, srv, "/nodes/n2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"n2", "10.0.0.2:7447", "Healthy", "serving node", `https://10.0.0.2:8443/nodes/n2`} {
		if !strings.Contains(body, want) {
			t.Errorf("other-node detail missing %q", want)
		}
	}
	if strings.Contains(body, "Reconciler status") {
		t.Error("other-node detail must not show the serving node's reconciler as if it were n2's")
	}
}

func TestNodeDetailUnknownNodeIs404(t *testing.T) {
	srv, pw, _ := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	resp, _ := getPage(t, client, srv, "/nodes/ghost")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestNodesEventsSSEStreamsLiveUpdate(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	stream := openSSE(t, client, srv.URL+"/nodes/events")
	stream.expect("event: nodes", 10*time.Second)
	putTestNode(t, st, join.NodeRecord{ID: "n9", RaftAddr: "10.0.0.9:7447", APIAddr: "10.0.0.9:7443"}, "")
	stream.expect("n9", 10*time.Second)
}

func TestNodeUIURLFallsBackToTheRaftHost(t *testing.T) {
	cases := []struct {
		n    control.NodeStatus
		want string
	}{
		{control.NodeStatus{ID: "n2", APIAddr: "10.0.0.2:7443", RaftAddr: "10.0.0.9:7444"}, "https://10.0.0.2:8443/nodes/n2"},
		{control.NodeStatus{ID: "n3", APIAddr: ":7443", RaftAddr: "192.168.1.3:7444"}, "https://192.168.1.3:8443/nodes/n3"},
		{control.NodeStatus{ID: "n1", RaftAddr: "192.168.1.1:7444"}, "https://192.168.1.1:8443/nodes/n1"},
		{control.NodeStatus{ID: "n4"}, ""},
	}
	for _, c := range cases {
		if got := nodeUIURL(c.n); got != c.want {
			t.Errorf("nodeUIURL(%+v) = %q, want %q", c.n, got, c.want)
		}
	}
}

// Node IDs may be UUIDs: pages name nodes by their published hostname, linking by ID.
func TestNodeListsShowPublishedHostnames(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	putTestNode(t, st, join.NodeRecord{ID: "n2", RaftAddr: "10.0.0.2:7447", APIAddr: "10.0.0.2:7443", Role: "voter"}, "health=healthy")
	putTestNode(t, st, join.NodeRecord{ID: "n3", RaftAddr: "10.0.0.3:7447", APIAddr: "10.0.0.3:7443", Role: "voter"}, "health=healthy")
	if _, err := st.Put(context.Background(), join.HostnameKey("n2"), []byte("rack1-b")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/cluster", "/nodes"} {
		_, body := getPage(t, client, srv, path)
		for _, want := range []string{`href="/nodes/n2"><strong>rack1-b</strong>`, `href="/nodes/n3"><strong>n3</strong>`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s missing %q", path, want)
			}
		}
	}
	if _, body := getPage(t, client, srv, "/nodes/n2"); !strings.Contains(body, `<h1 class="page-title">rack1-b `) {
		t.Errorf("node page not titled by hostname")
	}
	putTestVolume(t, st, storage.Spec{ID: "vol-h", Name: "hv", SizeBytes: 1 << 30, Class: "default", Replication: 1}, storage.Status{State: storage.StateHealthy, Primary: "n2"})
	if _, body := getPage(t, client, srv, "/health"); !strings.Contains(body, `<a href="/nodes/n2">rack1-b</a>`) {
		t.Errorf("health page's volume primary not named by hostname")
	}
}

// The Leader cards name the leader by hostname rather than its raft address.
func TestLeaderCardsNameTheLeaderNode(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	if _, err := st.Put(context.Background(), join.HostnameKey("n1"), []byte("rack1-a")); err != nil {
		t.Fatal(err)
	}
	card := `Leader</div><div class="kv-value"><a href="/nodes/n1"><strong>rack1-a</strong></a></div><div class="kv-sub">raft `
	for path, want := range map[string]string{"/": "leader rack1-a", "/cluster": card, "/health": card} {
		if _, body := getPage(t, client, srv, path); !strings.Contains(body, want) {
			t.Errorf("%s missing %q", path, want)
		}
	}
}
