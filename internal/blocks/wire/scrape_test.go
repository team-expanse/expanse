package wire

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/blocks/controller"
	"github.com/expanse/expanse/internal/blocks/runtime/systemd"
	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

func putJSON(t *testing.T, ctx context.Context, st *raftstore.Store, key string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key(key), raw); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

// seedPrometheus places one monitor/prometheus replica on n1.
func seedPrometheus(t *testing.T, ctx context.Context, st *raftstore.Store) {
	t.Helper()
	blk, _ := proto.Marshal(&pb.Block{
		Metadata: &pb.Metadata{Name: "prom", Namespace: "default"},
		Spec:     &pb.BlockSpec{Type: "monitor/prometheus"},
	})
	status, _ := proto.Marshal(&pb.BlockStatus{Placements: []*pb.PlacementStatus{
		{ReplicaIndex: 0, NodeId: "n1", Phase: pb.Phase_RUNNING},
	}})
	if _, err := st.Put(ctx, "/blocks/default/prom", blk); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, "/blocks/default/prom/status", status); err != nil {
		t.Fatal(err)
	}
}

func specArgs(t *testing.T, ctx context.Context, st *raftstore.Store, key string) []string {
	t.Helper()
	e, err := st.Get(ctx, store.Key(key))
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	_, body, _ := strings.Cut(string(e.Value), "\n")
	var spec systemd.Spec
	if err := json.Unmarshal([]byte(body), &spec); err != nil {
		t.Fatal(err)
	}
	return spec.Args
}

func argValues(args []string, flag string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			out = append(out, args[i+1])
		}
	}
	return out
}

func TestBridgeHandsPrometheusEveryVoterAndTheCABundle(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	seedPrometheus(t, ctx, st)
	putJSON(t, ctx, st, join.NodesKeyPrefix+"n2", join.NodeRecord{ID: "n2", Role: "voter", RaftAddr: "10.0.0.2:7444"})
	putJSON(t, ctx, st, join.NodesKeyPrefix+"n1", join.NodeRecord{ID: "n1", Role: "voter", RaftAddr: "10.0.0.1:7444"})
	putJSON(t, ctx, st, join.NodesKeyPrefix+"w1", join.NodeRecord{ID: "w1", Role: "witness", RaftAddr: "10.0.0.9:7444"})
	putJSON(t, ctx, st, control.CATrustKey, control.CATrust{
		Primary:  control.CAEntry{CertPEM: []byte("NEW-CA\n")},
		Outgoing: &control.CAEntry{CertPEM: []byte("OLD-CA\n")},
	})

	if err := (&Bridge{St: st}).Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	args := specArgs(t, ctx, st, "/node/n1/resources/"+controller.ReplicaResourceID("default", "prom", 0))

	if got := argValues(args, "--scrape-node"); strings.Join(got, ",") != "n1=10.0.0.1,n2=10.0.0.2" {
		t.Errorf("scrape nodes = %v, want sorted voters n1, n2 without the witness", got)
	}
	ca := argValues(args, "--scrape-ca")
	if len(ca) != 1 || ca[0] != "NEW-CA\nOLD-CA\n" {
		t.Errorf("scrape CA = %q, want both certs so a rotation keeps verifying", ca)
	}
}

func TestBridgeGivesOtherTypesNoScrapeArgs(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	seedPlaced(t, ctx, st, 18080)
	putJSON(t, ctx, st, join.NodesKeyPrefix+"n1", join.NodeRecord{ID: "n1", Role: "voter", RaftAddr: "10.0.0.1:7444"})

	if err := (&Bridge{St: st}).Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	args := specArgs(t, ctx, st, "/node/n1/resources/"+controller.ReplicaResourceID("default", "web", 0))
	if n := argValues(args, "--scrape-node"); len(n) != 0 {
		t.Errorf("util/echo got scrape args: %v", args)
	}
}
