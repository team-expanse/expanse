package agent

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

func TestProbeJobsCoverThisNodesReplicas(t *testing.T) {
	ctx := context.Background()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("boltstore.New: %v", err)
	}
	defer st.Close()
	web, _ := proto.Marshal(&pb.Block{
		Metadata: &pb.Metadata{Name: "web", Namespace: "default"},
		Spec: &pb.BlockSpec{Type: "web/whoami", Network: &pb.Network{HealthCheck: &pb.HealthCheck{
			Readiness: &pb.HealthProbe{Type: pb.ProbeType_PROBE_TCP, Port: 8080},
		}}},
	})
	for k, v := range map[string]string{
		"/nodes/n1": `{"raft_addr":"10.0.0.5:7000"}`,
		"/node/n1/resources/block-replica:default/web/2":  "{}",
		"/node/n1/resources/block-replica:default/gone/0": "{}", // block deleted since
		"/node/n1/resources/volume:data":                  "{}",
		"/node/n2/resources/block-replica:default/web/0":  "{}",
		"/blocks/default/web":                             string(web),
	} {
		if _, err := st.Put(ctx, store.Key(k), []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	a := newTestAgent(t, st)
	a.cfg.NodeID = "n1"
	a.nodeAddr = map[string]nodeAddr{}

	jobs := a.probeJobs(ctx)
	if len(jobs) != 1 || jobs[0].ID() != "/blocks/default/web/2" || jobs[0].Target.Address != "10.0.0.5:8080" {
		t.Fatalf("jobs = %+v, want web/2 probed at 10.0.0.5:8080", jobs)
	}
}

func TestReplicaRef(t *testing.T) {
	ns, name, idx, ok := replicaRef("/node/n1/resources/block-replica:default/web/2")
	if !ok || ns != "default" || name != "web" || idx != 2 {
		t.Errorf("got %q %q %d %t", ns, name, idx, ok)
	}
	for _, k := range []string{
		"/node/n1/resources/volume:data",
		"/node/n1/resources/block-replica:default/web",
		"/node/n1/resources/block-replica:default/web/x",
	} {
		if _, _, _, ok := replicaRef(k); ok {
			t.Errorf("%s parsed as a replica", k)
		}
	}
}
