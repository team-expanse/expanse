package wire

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"testing"

	expmount "github.com/expanse/expanse/internal/storage/mount"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// mountResource syncs and returns n1's volume-mount resource for the seeded block, retyped first.
func mountResource(t *testing.T, typ string) expmount.Resource {
	t.Helper()
	st := newStore(t)
	ctx := context.Background()
	seedPlaced(t, ctx, st, 18080)
	seedVolume(t, ctx, st, "n1")
	e, err := st.Get(ctx, store.Key("/blocks/default/web"))
	if err != nil {
		t.Fatal(err)
	}
	var blk pb.Block
	if err := proto.Unmarshal(e.Value, &blk); err != nil {
		t.Fatal(err)
	}
	blk.Spec.Type = typ
	out, err := proto.Marshal(&blk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key("/blocks/default/web"), out); err != nil {
		t.Fatal(err)
	}
	if err := (&Bridge{St: st}).Sync(ctx); err != nil {
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
	return res
}

// git never fsyncs a directory after renaming a ref into place, so Forgejo's volume needs dirsync.
func TestBridgeMountsForgejoVolumesWithDirsync(t *testing.T) {
	if res := mountResource(t, "dev/forgejo"); !slices.Equal(res.Options, []string{"dirsync"}) {
		t.Errorf("dev/forgejo mount options = %v, want [dirsync]", res.Options)
	}
	if res := mountResource(t, "util/echo"); len(res.Options) != 0 {
		t.Errorf("util/echo mount options = %v, want none", res.Options)
	}
}
