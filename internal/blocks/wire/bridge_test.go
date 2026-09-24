package wire

// Bridge (T20.5b) tests: placements → per-node desired-state keys.
import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/blocks/runtime/systemd"
	expstorage "github.com/expanse/expanse/internal/storage"
	expmount "github.com/expanse/expanse/internal/storage/mount"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// seedPlaced writes a block + status with placements: idx 0 on n1
// (RUNNING), idx 1 on n2 (SCHEDULING), idx 2 retired.
func seedPlaced(t *testing.T, ctx context.Context, st *raftstore.Store, port float64) {
	t.Helper()
	r2 := int32(2)
	blk, err := proto.Marshal(&pb.Block{
		Metadata: &pb.Metadata{Name: "web", Namespace: "default"},
		Spec: &pb.BlockSpec{
			Type:     "util/echo",
			Replicas: &r2,
			Config:   structOf(t, map[string]any{"port": port}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key("/blocks/default/web"), blk); err != nil {
		t.Fatalf("put block: %v", err)
	}
	status, err := proto.Marshal(&pb.BlockStatus{
		Phase: pb.Phase_SCHEDULING,
		Placements: []*pb.PlacementStatus{
			{ReplicaIndex: 0, NodeId: "n1", Phase: pb.Phase_RUNNING},
			{ReplicaIndex: 1, NodeId: "n2", Phase: pb.Phase_SCHEDULING},
			{ReplicaIndex: -1, NodeId: "n1", Phase: pb.Phase_LOST},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key("/blocks/default/web/status"), status); err != nil {
		t.Fatalf("put status: %v", err)
	}
}

func structOf(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	v, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestBridgeSyncWritesSpecs(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	seedPlaced(t, ctx, st, 18080)
	b := &Bridge{St: st}
	if err := b.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Two live placements → two desired specs; the retired record gets
	// none.
	for _, want := range []struct {
		node  string
		index int
	}{{"n1", 0}, {"n2", 1}} {
		key := "/node/" + want.node + "/resources/block-replica:default/web/" +
			strconv.Itoa(want.index)
		e, err := st.Get(ctx, store.Key(key))
		if err != nil {
			t.Fatalf("missing desired key %s: %v", key, err)
		}
		payload, ok := bytes.CutPrefix(e.Value, []byte("type: "+systemd.TypeBlockReplica+"\n"))
		if !ok {
			t.Fatalf("spec %s: missing canonical header: %q", key, e.Value)
		}
		var spec systemd.Spec
		if err := json.Unmarshal(payload, &spec); err != nil {
			t.Fatalf("spec %s: %v", key, err)
		}
		if spec.Namespace != "default" || spec.Name != "web" || spec.Index != want.index {
			t.Errorf("spec identity wrong: %+v", spec)
		}
		if spec.Type != "util/echo" {
			t.Errorf("spec type = %q", spec.Type)
		}
		if len(spec.Args) != 2 || spec.Args[0] != "--config" ||
			!strings.Contains(spec.Args[1], "18080") {
			t.Errorf("spec args = %v, want --config {…port…}", spec.Args)
		}
	}
	if _, err := st.Get(ctx, store.Key("/node/n1/resources/block-replica:default/web/2")); err == nil {
		t.Error("retired placement got a desired key")
	}
}

// Sync is idempotent and updates on change, deletes on removal.
func TestBridgeSyncConverges(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	seedPlaced(t, ctx, st, 18080)
	b := &Bridge{St: st}
	if err := b.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	key := store.Key("/node/n1/resources/block-replica:default/web/0")
	before, err := st.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	// Idempotent: no rewrite churn.
	if err := b.Sync(ctx); err != nil {
		t.Fatalf("Sync 2: %v", err)
	}
	after, err := st.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if before.Revision != after.Revision {
		t.Errorf("in-sync key rewritten: rev %d → %d", before.Revision, after.Revision)
	}

	// Config change → the spec value updates.
	ctx2 := context.Background()
	seedPlaced(t, ctx2, st, 18081) // overwrites block+status with new config
	if err := b.Sync(ctx2); err != nil {
		t.Fatalf("Sync 3: %v", err)
	}
	updated, err := st.Get(ctx2, key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated.Value), "18081") {
		t.Errorf("spec not updated: %s", updated.Value)
	}

	// Placement removal → desired key deleted.
	e, err := st.Get(ctx2, store.Key("/blocks/default/web/status"))
	if err != nil {
		t.Fatal(err)
	}
	var status pb.BlockStatus
	if err := proto.Unmarshal(e.Value, &status); err != nil {
		t.Fatal(err)
	}
	status.Placements = status.Placements[:1] // drop idx 1 (n2)
	out, err := proto.Marshal(&status)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx2, store.Key("/blocks/default/web/status"), out); err != nil {
		t.Fatal(err)
	}
	if err := b.Sync(ctx2); err != nil {
		t.Fatalf("Sync 4: %v", err)
	}
	if _, err := st.Get(ctx2, store.Key("/node/n2/resources/block-replica:default/web/1")); err == nil {
		t.Error("desired key survived placement removal")
	}
	if _, err := st.Get(ctx2, key); err != nil {
		t.Error("live desired key was removed too")
	}
}

// seedVolume gives the placed block a storage entry backed by a volume whose
// primary is the given node. SINGLETON (PHASE-05-TASKS.md D3: this
// helper's callers all test the shared-volume-follows-primary wiring
// that only SINGLETON/DAEMONSET use; seedPlaced's own active-active
// shape would instead resolve each replica to its OWN volume name).
func seedVolume(t *testing.T, ctx context.Context, st *raftstore.Store, primary string) {
	t.Helper()
	e, err := st.Get(ctx, store.Key("/blocks/default/web"))
	if err != nil {
		t.Fatal(err)
	}
	var blk pb.Block
	if err := proto.Unmarshal(e.Value, &blk); err != nil {
		t.Fatal(err)
	}
	blk.Spec.Strategy = &pb.Strategy{Kind: pb.StrategyKind_SINGLETON}
	blk.Spec.Storage = []*pb.Storage{{Name: "data", MountPath: "/data"}}
	out, err := proto.Marshal(&blk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key("/blocks/default/web"), out); err != nil {
		t.Fatal(err)
	}
	// The volume lives under its auto-provisioned composite name
	// (expstorage.BlockVolumeName), the same name reconcileBlocks
	// creates it under — not the storage entry's own raw "data" name.
	vname := expstorage.BlockVolumeName("default", "web", "data")
	if err := expstorage.SaveSpec(ctx, st, expstorage.Spec{ID: "vol-1", Name: vname, Namespace: "default"}); err != nil {
		t.Fatal(err)
	}
	if err := expstorage.SaveStatus(ctx, st, "vol-1", expstorage.Status{Primary: primary}); err != nil {
		t.Fatal(err)
	}
}

// The bound volume's real HOST path reaches the workload as a "--mount
// name=path" arg (the only channel expanse-block-run has for it — the
// config JSON is the user's own schema-validated spec.config). It is the
// host path (expmount.HostPath), not the user's declared
// spec.storage[].mountPath: the shared static block unit
// (nix/modules/agent.nix) grants a single fixed ReadWritePaths for every
// replica of every type, so the host path is the only one a block's own
// sandboxed process can actually see — the declared mountPath only
// governs the separate host-level bind mount §4.7 performs. Gated on the
// same volume-readiness check as BindPaths itself, so a workload never
// starts believing a mount exists before the bind actually does.
func TestBridgeSpecCarriesTheVolumesRealHostPathAsAnArg(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	seedPlaced(t, ctx, st, 18080)
	seedVolume(t, ctx, st, "n1")
	b := &Bridge{St: st}
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	e, err := st.Get(ctx, store.Key("/node/n1/resources/block-replica:default/web/0"))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := bytes.CutPrefix(e.Value, []byte("type: "+systemd.TypeBlockReplica+"\n"))
	var spec systemd.Spec
	if err := json.Unmarshal(payload, &spec); err != nil {
		t.Fatal(err)
	}
	want := []string{"--mount", "data=" + expmount.HostPath("", "vol-1")}
	found := false
	for i := 0; i+1 < len(spec.Args); i++ {
		if spec.Args[i] == want[0] && spec.Args[i+1] == want[1] {
			found = true
		}
	}
	if !found {
		t.Errorf("spec.Args = %v, want a %v pair", spec.Args, want)
	}
}

// No volume yet (unhealthy or not yet provisioned): the mount arg must
// not appear, matching BindPaths' own gate — a workload started with a
// mount path that isn't actually bound would write into a throwaway
// sandbox directory instead of the replicated volume.
func TestBridgeOmitsTheMountArgUntilTheVolumeExists(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	seedPlaced(t, ctx, st, 18080)
	e, err := st.Get(ctx, store.Key("/blocks/default/web"))
	if err != nil {
		t.Fatal(err)
	}
	var blk pb.Block
	if err := proto.Unmarshal(e.Value, &blk); err != nil {
		t.Fatal(err)
	}
	blk.Spec.Storage = []*pb.Storage{{Name: "data", MountPath: "/data"}}
	out, err := proto.Marshal(&blk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key("/blocks/default/web"), out); err != nil {
		t.Fatal(err)
	}
	b := &Bridge{St: st}
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	e2, err := st.Get(ctx, store.Key("/node/n1/resources/block-replica:default/web/0"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(e2.Value, []byte("--mount")) {
		t.Errorf("mount arg present before the volume exists: %s", e2.Value)
	}
}

func hasKey(ctx context.Context, st *raftstore.Store, key string) bool {
	_, err := st.Get(ctx, store.Key(key))
	return err == nil
}

// The mount resource follows the volume's primary: it is written on that node
// only and removed from a node the primary has left.
func TestBridgeMountsTheVolumeOnItsPrimaryOnly(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	seedPlaced(t, ctx, st, 18080)
	seedVolume(t, ctx, st, "n1")
	b := &Bridge{St: st}
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	mountKey := func(node string) string { return "/node/" + node + "/resources/" + expmount.Type + ":vol-1" }
	if !hasKey(ctx, st, mountKey("n1")) || hasKey(ctx, st, mountKey("n2")) {
		t.Fatalf("want the mount on n1 only (n1=%v n2=%v)", hasKey(ctx, st, mountKey("n1")), hasKey(ctx, st, mountKey("n2")))
	}

	if err := expstorage.SaveStatus(ctx, st, "vol-1", expstorage.Status{Primary: "n2"}); err != nil {
		t.Fatal(err)
	}
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if hasKey(ctx, st, mountKey("n1")) || !hasKey(ctx, st, mountKey("n2")) {
		t.Fatalf("the mount must follow the primary to n2 (n1=%v n2=%v)", hasKey(ctx, st, mountKey("n1")), hasKey(ctx, st, mountKey("n2")))
	}
}

// TestBridgeAttachResourceUsesTheStorageEntrysDeclaredFilesystem is the
// regression test for PHASE-04-TASKS.md D3: the §4.7 host-level attach
// resource must carry the block's own declared filesystem, not a hardcoded
// "ext4" that silently ignores it (the bug that made mount.Resource's
// "none" raw path dead code — nothing ever called it).
func TestBridgeAttachResourceUsesTheStorageEntrysDeclaredFilesystem(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	seedPlaced(t, ctx, st, 18080)
	e, err := st.Get(ctx, store.Key("/blocks/default/web"))
	if err != nil {
		t.Fatal(err)
	}
	var blk pb.Block
	if err := proto.Unmarshal(e.Value, &blk); err != nil {
		t.Fatal(err)
	}
	// SINGLETON: this test is about filesystem propagation onto the
	// shared-volume attach resource, not D3's active-active per-replica
	// naming, so it seeds the volume under the shared composite name.
	blk.Spec.Strategy = &pb.Strategy{Kind: pb.StrategyKind_SINGLETON}
	blk.Spec.Storage = []*pb.Storage{{Name: "data", MountPath: "/data", Filesystem: "none"}}
	out, err := proto.Marshal(&blk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key("/blocks/default/web"), out); err != nil {
		t.Fatal(err)
	}
	vname := expstorage.BlockVolumeName("default", "web", "data")
	if err := expstorage.SaveSpec(ctx, st, expstorage.Spec{ID: "vol-1", Name: vname, Namespace: "default"}); err != nil {
		t.Fatal(err)
	}
	if err := expstorage.SaveStatus(ctx, st, "vol-1", expstorage.Status{Primary: "n1"}); err != nil {
		t.Fatal(err)
	}
	b := &Bridge{St: st}
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	e2, err := st.Get(ctx, store.Key("/node/n1/resources/"+expmount.Type+":vol-1"))
	if err != nil {
		t.Fatal(err)
	}
	_, payload, _ := bytes.Cut(e2.Value, []byte("\n"))
	var res expmount.Resource
	if err := json.Unmarshal(payload, &res); err != nil {
		t.Fatal(err)
	}
	if res.Filesystem != "none" {
		t.Errorf("Filesystem = %q, want the block's own declared %q", res.Filesystem, "none")
	}
}

// TestBridgeAttachResourceDefaultsToExt4 confirms the pre-D3 hardcoded
// behavior is preserved for a block that declares no filesystem at all.
func TestBridgeAttachResourceDefaultsToExt4(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	seedPlaced(t, ctx, st, 18080)
	seedVolume(t, ctx, st, "n1")
	b := &Bridge{St: st}
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	e, err := st.Get(ctx, store.Key("/node/n1/resources/"+expmount.Type+":vol-1"))
	if err != nil {
		t.Fatal(err)
	}
	_, payload, _ := bytes.Cut(e.Value, []byte("\n"))
	var res expmount.Resource
	if err := json.Unmarshal(payload, &res); err != nil {
		t.Fatal(err)
	}
	if res.Filesystem != "ext4" {
		t.Errorf("Filesystem = %q, want the default %q", res.Filesystem, "ext4")
	}
}

// TestBridgeOmitsTheDirectoryBindForARawStorageEntry is the regression test
// for PHASE-04-TASKS.md D3's other half: a raw entry's host path is never
// mounted (mount.Manager.attach skips it), so bind-mounting that always-
// empty directory into the workload's sandbox would be actively misleading,
// not merely incomplete — the workload needs the device itself (Stream B),
// not a directory bind this phase does not yet provide.
func TestBridgeOmitsTheDirectoryBindForARawStorageEntry(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	seedPlaced(t, ctx, st, 18080)
	e, err := st.Get(ctx, store.Key("/blocks/default/web"))
	if err != nil {
		t.Fatal(err)
	}
	var blk pb.Block
	if err := proto.Unmarshal(e.Value, &blk); err != nil {
		t.Fatal(err)
	}
	// SINGLETON: this test is about the raw-entry mount-omission rule,
	// not D3's active-active per-replica naming, so it seeds the volume
	// under the shared composite name.
	blk.Spec.Strategy = &pb.Strategy{Kind: pb.StrategyKind_SINGLETON}
	blk.Spec.Storage = []*pb.Storage{{Name: "data", MountPath: "/data", Filesystem: "none"}}
	out, err := proto.Marshal(&blk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key("/blocks/default/web"), out); err != nil {
		t.Fatal(err)
	}
	vname := expstorage.BlockVolumeName("default", "web", "data")
	if err := expstorage.SaveSpec(ctx, st, expstorage.Spec{ID: "vol-1", Name: vname, Namespace: "default"}); err != nil {
		t.Fatal(err)
	}
	if err := expstorage.SaveStatus(ctx, st, "vol-1", expstorage.Status{Primary: "n1"}); err != nil {
		t.Fatal(err)
	}
	b := &Bridge{St: st}
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	e2, err := st.Get(ctx, store.Key("/node/n1/resources/block-replica:default/web/0"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(e2.Value, []byte("--mount")) {
		t.Errorf("--mount arg present for a raw storage entry: %s", e2.Value)
	}
}

// TestBridgeVoldevArgForARawStorageEntry confirms the other half of D3's
// raw-entry handling (PHASE-04-TASKS.md Stream B): since there is no host
// mount to bind, the leader-side bridge instead hands the workload the
// bound volume's stable ID via --voldev, for it to resolve its own
// device path locally (waitForPrimaryDevice, cmd/expanse-block-run) once
// it actually holds DRBD Primary.
func TestBridgeVoldevArgForARawStorageEntry(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	seedPlaced(t, ctx, st, 18082)
	e, err := st.Get(ctx, store.Key("/blocks/default/web"))
	if err != nil {
		t.Fatal(err)
	}
	var blk pb.Block
	if err := proto.Unmarshal(e.Value, &blk); err != nil {
		t.Fatal(err)
	}
	// SINGLETON: this test is about the raw-entry --voldev arg, not D3's
	// active-active per-replica naming, so it seeds the volume under the
	// shared composite name.
	blk.Spec.Strategy = &pb.Strategy{Kind: pb.StrategyKind_SINGLETON}
	blk.Spec.Storage = []*pb.Storage{{Name: "data", MountPath: "/data", Filesystem: "none"}}
	out, err := proto.Marshal(&blk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key("/blocks/default/web"), out); err != nil {
		t.Fatal(err)
	}
	vname := expstorage.BlockVolumeName("default", "web", "data")
	if err := expstorage.SaveSpec(ctx, st, expstorage.Spec{ID: "vol-1", Name: vname, Namespace: "default"}); err != nil {
		t.Fatal(err)
	}
	if err := expstorage.SaveStatus(ctx, st, "vol-1", expstorage.Status{Primary: "n1"}); err != nil {
		t.Fatal(err)
	}
	b := &Bridge{St: st}
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	e2, err := st.Get(ctx, store.Key("/node/n1/resources/block-replica:default/web/0"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(e2.Value, []byte("--voldev\",\"data=vol-1\"")) {
		t.Errorf("--voldev data=vol-1 arg missing for a raw storage entry: %s", e2.Value)
	}
}

// Regression test: a volume named after the storage entry's own raw name
// ("data") instead of its auto-provisioned composite name
// (expstorage.BlockVolumeName) must NOT be picked up — this was the actual
// bug PHASE-03-TASKS.md's Stream A1 found: reconcileBlocks
// (internal/storage/controller) creates the volume under the composite
// name, but the bridge's own lookup used the raw name, so no VM test ever
// exercised the mount path end to end.
func TestBridgeIgnoresAVolumeNamedAfterTheRawStorageEntry(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	seedPlaced(t, ctx, st, 18081)
	e, err := st.Get(ctx, store.Key("/blocks/default/web"))
	if err != nil {
		t.Fatal(err)
	}
	var blk pb.Block
	if err := proto.Unmarshal(e.Value, &blk); err != nil {
		t.Fatal(err)
	}
	// SINGLETON: this test is about rejecting a wrongly-named volume
	// under the shared composite scheme, not D3's active-active
	// per-replica naming.
	blk.Spec.Strategy = &pb.Strategy{Kind: pb.StrategyKind_SINGLETON}
	blk.Spec.Storage = []*pb.Storage{{Name: "data", MountPath: "/data"}}
	out, err := proto.Marshal(&blk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key("/blocks/default/web"), out); err != nil {
		t.Fatal(err)
	}
	if err := expstorage.SaveSpec(ctx, st, expstorage.Spec{ID: "vol-1", Name: "data", Namespace: "default"}); err != nil {
		t.Fatal(err)
	}
	if err := expstorage.SaveStatus(ctx, st, "vol-1", expstorage.Status{Primary: "n1"}); err != nil {
		t.Fatal(err)
	}
	b := &Bridge{St: st}
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if hasKey(ctx, st, "/node/n1/resources/"+expmount.Type+":vol-1") {
		t.Fatal("mounted a volume named after the raw storage entry name, not its composite name")
	}
}

// TestBridgeActiveActiveReplicasMountTheirOwnIndependentVolumes is the
// regression test for PHASE-05-TASKS.md D3: an active-active block's
// storage entry must resolve to one INDEPENDENT volume per replica
// index, not the single shared composite name SINGLETON/DAEMONSET
// blocks use — otherwise every replica but the volume's primary-holding
// one would get no mount at all.
func TestBridgeActiveActiveReplicasMountTheirOwnIndependentVolumes(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	seedPlaced(t, ctx, st, 18080) // active-active, idx 0 on n1, idx 1 on n2
	e, err := st.Get(ctx, store.Key("/blocks/default/web"))
	if err != nil {
		t.Fatal(err)
	}
	var blk pb.Block
	if err := proto.Unmarshal(e.Value, &blk); err != nil {
		t.Fatal(err)
	}
	blk.Spec.Storage = []*pb.Storage{{Name: "data", MountPath: "/data"}}
	out, err := proto.Marshal(&blk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key("/blocks/default/web"), out); err != nil {
		t.Fatal(err)
	}
	v0 := expstorage.BlockReplicaVolumeName("default", "web", "data", 0)
	v1 := expstorage.BlockReplicaVolumeName("default", "web", "data", 1)
	if err := expstorage.SaveSpec(ctx, st, expstorage.Spec{ID: "vol-0", Name: v0, Namespace: "default"}); err != nil {
		t.Fatal(err)
	}
	if err := expstorage.SaveStatus(ctx, st, "vol-0", expstorage.Status{Primary: "n1"}); err != nil {
		t.Fatal(err)
	}
	if err := expstorage.SaveSpec(ctx, st, expstorage.Spec{ID: "vol-1", Name: v1, Namespace: "default"}); err != nil {
		t.Fatal(err)
	}
	if err := expstorage.SaveStatus(ctx, st, "vol-1", expstorage.Status{Primary: "n2"}); err != nil {
		t.Fatal(err)
	}
	b := &Bridge{St: st}
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if !hasKey(ctx, st, "/node/n1/resources/"+expmount.Type+":vol-0") {
		t.Error("replica 0 (n1) never got its own volume's mount resource")
	}
	if !hasKey(ctx, st, "/node/n2/resources/"+expmount.Type+":vol-1") {
		t.Error("replica 1 (n2) never got its own volume's mount resource")
	}
	// Cross-wiring would be worse than no wiring at all: replica 0 must
	// never be handed replica 1's volume, or vice versa.
	if hasKey(ctx, st, "/node/n1/resources/"+expmount.Type+":vol-1") {
		t.Error("replica 0 (n1) was wired to replica 1's volume")
	}
	if hasKey(ctx, st, "/node/n2/resources/"+expmount.Type+":vol-0") {
		t.Error("replica 1 (n2) was wired to replica 0's volume")
	}
}
