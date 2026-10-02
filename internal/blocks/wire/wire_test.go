package wire

// Nodes adapter tests: single-node raftstore (the T16 harness pattern)
// with a second node record written directly.
import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/scheduler"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

func newStore(t *testing.T) *raftstore.Store {
	t.Helper()
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.Close()
	st, err := raftstore.Open(raftstore.Config{
		NodeID: "n1", BindAddr: ln.Addr().String(), DataDir: dir,
		Bootstrap: true, LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for i := 0; i < 500 && st.Leader() == ""; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if st.Leader() == "" {
		t.Fatal("no leader")
	}
	return st
}

func TestNodesView(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Node records + statuses: n1 healthy, n2 no status yet.
	for _, pair := range [][2]string{
		{"/nodes/n1", `{"id":"n1"}`},
		{"/nodes/n2", `{"id":"n2"}`},
		{"/nodes/n1/status", "idle"},
	} {
		if _, err := st.Put(ctx, store.Key(pair[0]), []byte(pair[1])); err != nil {
			t.Fatalf("put %s: %v", pair[0], err)
		}
	}

	// A block with one active placement on n1 and one retired record.
	r2 := int32(2)
	block, err := proto.Marshal(&pb.Block{
		Metadata: &pb.Metadata{Name: "web", Namespace: "default"},
		Spec: &pb.BlockSpec{
			Type:     "util/echo",
			Replicas: &r2,
			Resources: &pb.Resources{Requests: &pb.ResourcePair{
				Cpu: "500m", Memory: "256Mi",
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key("/blocks/default/web"), block); err != nil {
		t.Fatalf("put block: %v", err)
	}
	status, err := proto.Marshal(&pb.BlockStatus{
		Phase: pb.Phase_RUNNING,
		Placements: []*pb.PlacementStatus{
			{ReplicaIndex: 0, NodeId: "n1", Phase: pb.Phase_RUNNING},
			{ReplicaIndex: -1, NodeId: "n2", Phase: pb.Phase_LOST},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key("/blocks/default/web/status"), status); err != nil {
		t.Fatalf("put status: %v", err)
	}

	views, cfg, err := Nodes(st)(ctx)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	byID := map[string]schedulerNode{}
	for _, v := range views {
		if v.CapacityCPU != DefaultCapacity.CPU {
			t.Errorf("%s CapacityCPU = %s, want %s: least-loaded scoring needs it", v.ID, v.CapacityCPU, DefaultCapacity.CPU)
		}
		byID[v.ID] = schedulerNode{
			Ready: v.Ready, FreeCPU: v.FreeCPU.Milli, FreeMem: v.FreeMem.N,
		}
	}
	if len(views) != 2 {
		t.Fatalf("views = %d, want 2: %+v", len(views), views)
	}
	if !byID["n1"].Ready || byID["n2"].Ready {
		t.Errorf("ready flags wrong: n1=%t n2=%t", byID["n1"].Ready, byID["n2"].Ready)
	}
	if got := byID["n1"].FreeCPU; got != DefaultCapacity.CPU.Milli-500 {
		t.Errorf("n1 FreeCPU = %d, want %d (500m subtracted)", got, DefaultCapacity.CPU.Milli-500)
	}
	wantMem := DefaultCapacity.Mem.N - 256*1024*1024
	if got := byID["n1"].FreeMem; got != wantMem {
		t.Errorf("n1 FreeMem = %d, want %d", got, wantMem)
	}
	// The retired record must hold no capacity on n2.
	if got := byID["n2"].FreeCPU; got != DefaultCapacity.CPU.Milli {
		t.Errorf("n2 FreeCPU = %d, want full (retired placement holds nothing)", got)
	}
	if cfg.CPUOvercommitRatio != 2.0 || cfg.MemoryOvercommitRatio != 1.0 {
		t.Errorf("default overcommit config wrong: %+v", cfg)
	}
}

// Placement follows the agent's schedulable= verdict, not its overall health (which counts advisory
// checks such as clock sync); health=healthy alone is how an agent from before schedulable= says ready.
func TestNodeReadyFollowsSchedulable(t *testing.T) {
	cases := map[string]bool{
		"health=unhealthy schedulable=true":                            true,
		"health=healthy schedulable=false":                             false,
		"health=healthy schedulable=true degraded=true writable=false": false,
		"health=healthy": true,
		"health=unknown": false,
		"idle":           true,
	}
	for value, want := range cases {
		st := newStore(t)
		if _, err := st.Put(context.Background(), "/nodes/n1/status", []byte(value)); err != nil {
			t.Fatal(err)
		}
		if got, err := nodeReady(context.Background(), st, "n1"); err != nil || got != want {
			t.Errorf("nodeReady(%q) = %t (err %v), want %t", value, got, err, want)
		}
	}
}

// TestNodesViewCapabilities is the regression test for PHASE-06-TASKS.md
// Stream A's own X1 test finding: a node's real hardware capabilities
// (internal/agent.Agent.refreshInventory's own publish) must reach the
// scheduler's NodeView, or placement.requiredCapabilities can never be
// satisfied by any node — P6 always rejects with "missing capability"
// regardless of actual hardware, since NodeView.Capabilities previously
// came from nowhere in production (only the scheduler's own unit tests
// ever set it directly).
func TestNodesViewCapabilities(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, pair := range [][2]string{
		{"/nodes/n1", `{"id":"n1"}`},
		{"/nodes/n1/status", "idle"},
		{"/nodes/n1/capabilities", "kvm,aes-ni"},
		{"/nodes/n2", `{"id":"n2"}`},
		{"/nodes/n2/status", "idle"},
	} {
		if _, err := st.Put(ctx, store.Key(pair[0]), []byte(pair[1])); err != nil {
			t.Fatalf("put %s: %v", pair[0], err)
		}
	}
	views, _, err := Nodes(st)(ctx)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	byID := map[string][]string{}
	for _, v := range views {
		byID[v.ID] = v.Capabilities
	}
	got := byID["n1"]
	if len(got) != 2 || got[0] != "kvm" || got[1] != "aes-ni" {
		t.Errorf("n1 Capabilities = %v, want [kvm aes-ni]", got)
	}
	if len(byID["n2"]) != 0 {
		t.Errorf("n2 Capabilities = %v, want empty (never published)", byID["n2"])
	}
}

// /config/scheduler overrides the defaults.
func TestOvercommitConfigFromStore(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if _, err := st.Put(ctx, store.Key("/config/scheduler"),
		[]byte(`{"cpuOvercommitRatio":4,"reservedCpu":"250m","reservedMemory":"512Mi"}`)); err != nil {
		t.Fatalf("put config: %v", err)
	}
	_, cfg, err := Nodes(st)(ctx)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	if cfg.CPUOvercommitRatio != 4.0 {
		t.Errorf("ratio = %v, want 4", cfg.CPUOvercommitRatio)
	}
	if cfg.ReservedCPU.Milli != 250 || cfg.ReservedMemory.N != 512*1024*1024 {
		t.Errorf("reserves wrong: %+v", cfg)
	}
}

// TestNodesViewHealthyVolumes is the regression test for PHASE-03-TASKS.md
// D2/P12: Volumes (S3, any replica) and HealthyVolumes (P12, healthy only)
// must diverge when a node's replica is unhealthy.
func TestNodesViewHealthyVolumes(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	for _, pair := range [][2]string{
		{"/nodes/n1", `{"id":"n1"}`},
		{"/nodes/n2", `{"id":"n2"}`},
		{"/nodes/n1/status", "idle"},
		{"/nodes/n2/status", "idle"},
	} {
		if _, err := st.Put(ctx, store.Key(pair[0]), []byte(pair[1])); err != nil {
			t.Fatalf("put %s: %v", pair[0], err)
		}
	}

	spec, err := proto.Marshal(&pb.VolumeSpec{Id: "vol-1", Name: "share-data", SizeBytes: 1 << 30, Replication: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key("/volumes/vol-1/spec"), spec); err != nil {
		t.Fatalf("put spec: %v", err)
	}
	// n1 holds a healthy replica; n2 holds one, but unhealthy (resyncing).
	status, err := proto.Marshal(&pb.VolumeStatus{
		Placement: []*pb.Replica{
			{NodeId: "n1", Healthy: true},
			{NodeId: "n2", Healthy: false},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key("/volumes/vol-1/status"), status); err != nil {
		t.Fatalf("put status: %v", err)
	}

	views, _, err := Nodes(st)(ctx)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	byID := map[string]scheduler.NodeView{}
	for _, v := range views {
		byID[v.ID] = v
	}
	if !contains(byID["n1"].Volumes, "share-data") || !contains(byID["n1"].HealthyVolumes, "share-data") {
		t.Errorf("n1: Volumes=%v HealthyVolumes=%v, want both to include share-data",
			byID["n1"].Volumes, byID["n1"].HealthyVolumes)
	}
	if !contains(byID["n2"].Volumes, "share-data") || contains(byID["n2"].HealthyVolumes, "share-data") {
		t.Errorf("n2: Volumes=%v HealthyVolumes=%v, want Volumes only (replica unhealthy)",
			byID["n2"].Volumes, byID["n2"].HealthyVolumes)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// The adapter degrades to an empty view on an empty store.
func TestNodesEmptyStore(t *testing.T) {
	st := newStore(t)
	views, _, err := Nodes(st)(context.Background())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	if len(views) != 0 {
		t.Errorf("views = %d, want 0", len(views))
	}
}

// schedulerNode is the field projection the assertions use.
type schedulerNode struct {
	Ready   bool
	FreeCPU int64
	FreeMem int64
}
