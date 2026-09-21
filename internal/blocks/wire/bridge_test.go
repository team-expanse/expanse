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
// primary is the given node.
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
	if err := expstorage.SaveStatus(ctx, st, "vol-1", expstorage.Status{Primary: primary}); err != nil {
		t.Fatal(err)
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
