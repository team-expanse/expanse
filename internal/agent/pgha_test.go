package agent

// Regression tests for the D1 election scan's real-world wiring gap
// found running PHASE-05-TASKS.md Stream A's X1 VM test (db-postgres.nix):
// scanPostgresInstances required a placement already RUNNING before
// scanning it (a chicken-and-egg deadlock -- db/postgres can only reach
// RUNNING once pgha has already written a role decision) and resolved
// the block's own declared, sandbox-only spec.storage[].mountPath
// instead of the real host path bridge.go actually wires the workload
// to. Both were only ever exercised by unit tests that constructed a
// pgha.Instance directly, never by scanPostgresInstances itself.

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/expanse/expanse/internal/blocks/controller"
	"github.com/expanse/expanse/internal/blocks/pgha"
	"github.com/expanse/expanse/internal/blocks/runtime/systemd"
	"github.com/expanse/expanse/internal/reconcile"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func pgTestAgent(t *testing.T, st store.Store, nodeID string) *Agent {
	t.Helper()
	return &Agent{
		store:  st,
		logger: slog.Default(),
		cfg:    Config{NodeID: nodeID},
		recon:  reconcile.New(st, reconcile.Options{NodeID: nodeID}),
	}
}

// putPGBlock seeds a db/postgres block spec + status with one placement.
func putPGBlock(t *testing.T, st store.Store, ns, name, node string, idx int32, phase pb.Phase) {
	t.Helper()
	ctx := context.Background()
	cfg, err := structpb.NewStruct(map[string]any{"port": float64(5432)})
	if err != nil {
		t.Fatal(err)
	}
	blk, err := proto.Marshal(&pb.Block{
		Spec: &pb.BlockSpec{
			Type:    pgha.BlockType,
			Config:  cfg,
			Storage: []*pb.Storage{{Name: "pgdata", MountPath: "/var/lib/postgresql-data"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key("/blocks/"+ns+"/"+name), blk); err != nil {
		t.Fatal(err)
	}
	status, err := proto.Marshal(&pb.BlockStatus{
		Placements: []*pb.PlacementStatus{{ReplicaIndex: idx, NodeId: node, Phase: phase}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key("/blocks/"+ns+"/"+name+"/status"), status); err != nil {
		t.Fatal(err)
	}
}

// putReplicaResource seeds the desired-state record bridge.go writes for
// one node+replica, carrying the real host mount path as a "--mount"
// arg -- the same record pgReplicaMountPath reads.
func putReplicaResource(t *testing.T, st store.Store, node, ns, name string, idx int, realHostPath string) {
	t.Helper()
	spec, err := json.Marshal(systemd.Spec{
		Namespace: ns, Name: name, Index: idx, Type: pgha.BlockType,
		Args: []string{"--mount", "pgdata=" + realHostPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	key := store.Key("/node/" + node + "/resources/" + controller.ReplicaResourceID(ns, name, idx))
	if _, err := st.Put(context.Background(), key, append([]byte("type: block-replica\n"), spec...)); err != nil {
		t.Fatal(err)
	}
}

func TestScanPostgresInstancesCountsAnyLivePlacementNotJustRunning(t *testing.T) {
	st, err := boltstore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// SCHEDULING, not RUNNING: db/postgres can never itself reach RUNNING
	// before pgha has acted, so requiring RUNNING here would deadlock.
	putPGBlock(t, st, "default", "pg", "n1", 0, pb.Phase_SCHEDULING)
	putReplicaResource(t, st, "n1", "default", "pg", 0, "/var/lib/expanse/volumes/vol-a/mnt")

	a := pgTestAgent(t, st, "n1")
	got := a.scanPostgresInstances(context.Background())
	inst, ok := got["default/pg"]
	if !ok {
		t.Fatalf("a SCHEDULING placement was not scanned: %+v", got)
	}
	if inst.MountPath != "/var/lib/expanse/volumes/vol-a/mnt" {
		t.Errorf("MountPath = %q, want the real host path", inst.MountPath)
	}
}

func TestScanPostgresInstancesResolvesTheRealMountPathNotTheDeclaredOne(t *testing.T) {
	st, err := boltstore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	putPGBlock(t, st, "default", "pg", "n1", 0, pb.Phase_RUNNING)
	putReplicaResource(t, st, "n1", "default", "pg", 0, "/var/lib/expanse/volumes/vol-a/mnt")

	a := pgTestAgent(t, st, "n1")
	got := a.scanPostgresInstances(context.Background())
	inst, ok := got["default/pg"]
	if !ok {
		t.Fatal("instance not scanned")
	}
	if inst.MountPath == "/var/lib/postgresql-data" {
		t.Fatal("used the block's declared sandbox-only mountPath instead of the real host path")
	}
	if inst.MountPath != "/var/lib/expanse/volumes/vol-a/mnt" {
		t.Errorf("MountPath = %q, want the real host path", inst.MountPath)
	}
}

func TestScanPostgresInstancesSkipsUntilTheBridgeWiresTheRealMount(t *testing.T) {
	st, err := boltstore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Placement exists, but bridge.go hasn't written the block-replica
	// resource (and its --mount arg) for this node yet.
	putPGBlock(t, st, "default", "pg", "n1", 0, pb.Phase_SCHEDULING)

	a := pgTestAgent(t, st, "n1")
	got := a.scanPostgresInstances(context.Background())
	if _, ok := got["default/pg"]; ok {
		t.Fatal("scanned an instance with no real mount path resolved yet")
	}
}

func TestScanPostgresInstancesIgnoresOtherNodesPlacements(t *testing.T) {
	st, err := boltstore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	putPGBlock(t, st, "default", "pg", "n2", 0, pb.Phase_RUNNING)
	putReplicaResource(t, st, "n2", "default", "pg", 0, "/var/lib/expanse/volumes/vol-a/mnt")

	a := pgTestAgent(t, st, "n1")
	got := a.scanPostgresInstances(context.Background())
	if _, ok := got["default/pg"]; ok {
		t.Fatal("n1 scanned a replica placed on n2")
	}
}

func TestScanPostgresInstancesIgnoresALostPlacement(t *testing.T) {
	st, err := boltstore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	putPGBlock(t, st, "default", "pg", "n1", -1, pb.Phase_LOST)

	a := pgTestAgent(t, st, "n1")
	got := a.scanPostgresInstances(context.Background())
	if _, ok := got["default/pg"]; ok {
		t.Fatal("scanned a retired (LOST) placement")
	}
}
