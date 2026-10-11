package agent

import (
	"context"
	"encoding/json"
	"net/netip"
	"slices"
	"testing"

	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/network/mesh"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

func putFwPeer(t *testing.T, st store.Store, id, lan, overlay string) {
	t.Helper()
	ctx := context.Background()
	rec, err := json.Marshal(map[string]string{"raft_addr": lan + ":7444"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key(join.NodesKeyPrefix+id), rec); err != nil {
		t.Fatal(err)
	}
	wg, err := proto.Marshal(&pb.WireGuardPeer{NodeId: id, OverlayPrefix: overlay, Endpoint: lan + ":51820"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, mesh.PublicKeyKey(id), wg); err != nil {
		t.Fatal(err)
	}
}

// TestFwDesiredSkipsARevokedNode: a removed node whose mesh key lingers is not admitted.
func TestFwDesiredSkipsARevokedNode(t *testing.T) {
	st, err := boltstore.New(t.TempDir() + "/bolt.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	putFwPeer(t, st, "n2", "192.168.1.2", "10.42.2.0/24")
	putFwPeer(t, st, "n3", "192.168.1.3", "10.42.3.0/24")
	if err := st.Delete(ctx, store.Key(join.NodesKeyPrefix+"n3"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key(join.RevokedKeyPrefix+"n3"), []byte(`{"node_id":"n3"}`)); err != nil {
		t.Fatal(err)
	}

	d, err := (&Agent{store: st}).fwDesired(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"192.168.1.2", "10.42.2.1"} {
		if !slices.Contains(d.Peers, netip.MustParseAddr(want)) {
			t.Errorf("n2's %s not admitted: %v", want, d.Peers)
		}
	}
	for _, banned := range []string{"192.168.1.3", "10.42.3.1"} {
		if slices.Contains(d.Peers, netip.MustParseAddr(banned)) {
			t.Errorf("revoked n3's %s still admitted: %v", banned, d.Peers)
		}
	}
}
