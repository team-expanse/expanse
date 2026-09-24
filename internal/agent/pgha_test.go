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
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
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

// fakeMounted stubs checkDistinctMount for one test, standing in for
// whether the mount-attach reconcile resource (internal/storage/mount)
// has actually landed the real filesystem on top of the pre-mount host
// directory yet -- a fact a bare "--mount" arg's presence in the
// desired-state record does not, by itself, guarantee.
func fakeMounted(t *testing.T, mounted bool) {
	t.Helper()
	orig := checkDistinctMount
	checkDistinctMount = func(string) bool { return mounted }
	t.Cleanup(func() { checkDistinctMount = orig })
}

func TestScanPostgresInstancesCountsAnyLivePlacementNotJustRunning(t *testing.T) {
	st, err := boltstore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fakeMounted(t, true)
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
	fakeMounted(t, true)
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

// TestScanPostgresInstancesSkipsAPreMountShadowPath is the regression
// test for the bug found running the X1 VM test past the earlier fixes:
// the mount-attach resource converges independently of the desired-
// state record naming its path, so a "--mount" arg's mere presence does
// not mean the real filesystem has landed there yet. Writing a role
// file to the pre-mount host directory writes somewhere the real mount
// then silently shadows the moment it lands, exactly like
// cmd/expanse-block-run's own waitForMount already guards the workload
// side against (its doc comment: "reproduced directly: smbd's own
// state directory went missing exactly this way").
func TestScanPostgresInstancesSkipsAPreMountShadowPath(t *testing.T) {
	st, err := boltstore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fakeMounted(t, false)
	putPGBlock(t, st, "default", "pg", "n1", 0, pb.Phase_SCHEDULING)
	putReplicaResource(t, st, "n1", "default", "pg", 0, "/var/lib/expanse/volumes/vol-a/mnt")

	a := pgTestAgent(t, st, "n1")
	got := a.scanPostgresInstances(context.Background())
	if _, ok := got["default/pg"]; ok {
		t.Fatal("scanned an instance whose mount had not actually landed yet")
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

// fakeRunPromoteSQL stubs the psql exec seam for one test, standing in
// for a real postgres connection.
func fakeRunPromoteSQL(t *testing.T, fn func(ctx context.Context, sockDir string, port int32) ([]byte, error)) {
	t.Helper()
	orig := runPromoteSQL
	runPromoteSQL = fn
	t.Cleanup(func() { runPromoteSQL = orig })
}

func TestPgPromoteSucceedsOnAtResponse(t *testing.T) {
	var gotSock string
	var gotPort int32
	fakeRunPromoteSQL(t, func(_ context.Context, sockDir string, port int32) ([]byte, error) {
		gotSock, gotPort = sockDir, port
		return []byte("t\n"), nil
	})
	a := &Agent{}
	inst := pgha.Instance{BlockRef: "default/pg", MountPath: "/var/lib/expanse/volumes/vol-a/mnt", Port: 5432}
	if err := a.pgPromote(inst); err != nil {
		t.Fatalf("pgPromote: %v", err)
	}
	if want := "/var/lib/expanse/volumes/vol-a/mnt/.expanse-postgres/sock"; gotSock != want {
		t.Errorf("sockDir = %q, want %q", gotSock, want)
	}
	if gotPort != 5432 {
		t.Errorf("port = %d, want 5432", gotPort)
	}
}

func TestPgPromoteFailsWhenPsqlErrors(t *testing.T) {
	fakeRunPromoteSQL(t, func(context.Context, string, int32) ([]byte, error) {
		return []byte("psql: error: connection refused"), fmt.Errorf("exit status 2")
	})
	a := &Agent{}
	inst := pgha.Instance{BlockRef: "default/pg", MountPath: "/mnt", Port: 5432}
	if err := a.pgPromote(inst); err == nil {
		t.Fatal("pgPromote returned nil despite a psql error")
	}
}

func TestPgPromoteFailsWhenServerReportsFalse(t *testing.T) {
	// pg_promote() itself succeeds (no SQL error) but returns false --
	// e.g. the server was not actually in recovery, so nothing was
	// promoted despite a zero exit status.
	fakeRunPromoteSQL(t, func(context.Context, string, int32) ([]byte, error) {
		return []byte("f\n"), nil
	})
	a := &Agent{}
	inst := pgha.Instance{BlockRef: "default/pg", MountPath: "/mnt", Port: 5432}
	if err := a.pgPromote(inst); err == nil {
		t.Fatal("pgPromote returned nil despite pg_promote() reporting false")
	}
}

type reconfigureCall struct {
	host, user, password, dbname, sql string
	port                              int32
}

// fakeRunReconfigureSQL stubs the psql exec seam, recording every call
// in order.
func fakeRunReconfigureSQL(t *testing.T, results ...error) *[]reconfigureCall {
	t.Helper()
	var calls []reconfigureCall
	orig := runReconfigureSQL
	runReconfigureSQL = func(_ context.Context, host string, port int32, user, password, dbname, sql string) ([]byte, error) {
		calls = append(calls, reconfigureCall{host, user, password, dbname, sql, port})
		if len(calls)-1 < len(results) {
			if err := results[len(calls)-1]; err != nil {
				return []byte("psql: error: boom"), err
			}
		}
		return nil, nil
	}
	t.Cleanup(func() { runReconfigureSQL = orig })
	return &calls
}

// TestPgReconfigureCreatesSlotOnNewPrimaryThenRetargetsLocally is the
// regression test for the X2 VM test's real failure: promotion alone
// left a surviving standby permanently streaming from the dead
// primary, which with synchronous_standby_names active meant the new
// primary could never complete a write at all.
func TestPgReconfigureCreatesSlotOnNewPrimaryThenRetargetsLocally(t *testing.T) {
	calls := fakeRunReconfigureSQL(t)
	a := &Agent{}
	inst := pgha.Instance{
		BlockRef: "default/pg", MountPath: "/var/lib/expanse/volumes/vol-a/mnt",
		Port: 55432, Index: 2, ReplPassword: "repl-s3cret",
	}
	if err := a.pgReconfigure(inst, "192.168.1.9"); err != nil {
		t.Fatalf("pgReconfigure: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("runReconfigureSQL called %d times, want 2", len(*calls))
	}
	slotCall := (*calls)[0]
	if slotCall.host != "192.168.1.9" || slotCall.port != 55432 {
		t.Errorf("slot creation dialed %s:%d, want the new primary 192.168.1.9:55432", slotCall.host, slotCall.port)
	}
	if slotCall.user != "replicator" || slotCall.password != "repl-s3cret" {
		t.Errorf("slot creation authenticated as %q/%q, want replicator/repl-s3cret", slotCall.user, slotCall.password)
	}
	wantSlot := "expanse_default_pg_2"
	if !strings.Contains(slotCall.sql, wantSlot) {
		t.Errorf("slot creation SQL %q missing slot name %q", slotCall.sql, wantSlot)
	}
	retargetCall := (*calls)[1]
	if retargetCall.host != "/var/lib/expanse/volumes/vol-a/mnt/.expanse-postgres/sock" {
		t.Errorf("retarget dialed %q, want this replica's own local socket dir", retargetCall.host)
	}
	if retargetCall.user != "postgres" || retargetCall.password != "" {
		t.Errorf("retarget authenticated as %q/%q, want postgres over the local trust rule (no password)", retargetCall.user, retargetCall.password)
	}
	for _, want := range []string{"primary_conninfo", "192.168.1.9", wantSlot, "pg_reload_conf"} {
		if !strings.Contains(retargetCall.sql, want) {
			t.Errorf("retarget SQL %q missing %q", retargetCall.sql, want)
		}
	}
}

func TestPgReconfigureFailsWithoutRetargetingIfSlotCreationFails(t *testing.T) {
	calls := fakeRunReconfigureSQL(t, fmt.Errorf("connection refused"))
	a := &Agent{}
	inst := pgha.Instance{BlockRef: "default/pg", MountPath: "/mnt", Port: 55432, ReplPassword: "x"}
	if err := a.pgReconfigure(inst, "192.168.1.9"); err == nil {
		t.Fatal("pgReconfigure returned nil despite a failed slot creation")
	}
	if len(*calls) != 1 {
		t.Errorf("runReconfigureSQL called %d times, want 1 (must not retarget without a slot)", len(*calls))
	}
}

func TestPgReconfigureFailsIfLocalRetargetFails(t *testing.T) {
	calls := fakeRunReconfigureSQL(t, nil, fmt.Errorf("connection refused"))
	a := &Agent{}
	inst := pgha.Instance{BlockRef: "default/pg", MountPath: "/mnt", Port: 55432, ReplPassword: "x"}
	if err := a.pgReconfigure(inst, "192.168.1.9"); err == nil {
		t.Fatal("pgReconfigure returned nil despite a failed local retarget")
	}
	if len(*calls) != 2 {
		t.Errorf("runReconfigureSQL called %d times, want 2", len(*calls))
	}
}
