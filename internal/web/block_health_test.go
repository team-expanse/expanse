package web

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/store/boltstore"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

func renderBlockFragment(t *testing.T, b *pb.Block) string {
	t.Helper()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s, err := New("node-a", st, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := s.renderFragment(&buf, "block-fragment", b); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestBlockFragmentShowsReplicaHealthAndFailedNodes(t *testing.T) {
	probed := time.Now().Add(-10 * time.Second).UnixNano()
	out := renderBlockFragment(t, &pb.Block{
		Metadata: &pb.Metadata{Namespace: "default", Name: "web"},
		Spec:     &pb.BlockSpec{Type: "web/nginx", Replicas: proto.Int32(2)},
		Status: &pb.BlockStatus{
			Phase:    pb.Phase_DEGRADED,
			Replicas: &pb.StatusReplicas{Ready: 1},
			PendingReason: &pb.PendingReason{
				Code: "LivenessFailed", Message: "replica 1 failed its liveness probe on 2 nodes and was stopped",
			},
			Placements: []*pb.PlacementStatus{
				{ReplicaIndex: 0, NodeId: "node-a", Phase: pb.Phase_RUNNING, Health: &pb.ReplicaHealth{
					Readiness: &pb.ProbeResult{Ok: true, Detail: "200 OK", AtUnixNs: probed}, Restarts: 3,
				}},
				{ReplicaIndex: 1, NodeId: "node-b", Phase: pb.Phase_STARTING, Health: &pb.ReplicaHealth{
					Readiness: &pb.ProbeResult{Ok: false, Detail: "503 Service Unavailable", AtUnixNs: probed},
				}},
				{
					ReplicaIndex: -1, FormerIndex: 1, NodeId: "node-c", Phase: pb.Phase_FAILED,
					Message: "liveness probe failed after 5 restarts: refused",
				},
			},
		},
	})
	for _, want := range []string{
		"Restarts", "200 OK", "503 Service Unavailable", ">3<",
		"liveness probe failed after 5 restarts: refused", "LivenessFailed",
		`href="/nodes/node-c"`, "Stopped here",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fragment lacks %q", want)
		}
	}
	if strings.Contains(out, "replica=-1") {
		t.Error("a stopped placement links to logs of replica -1")
	}
	if strings.Count(out, "logs?replica=1") != 1 {
		t.Error("want one logs link for replica 1, on its live placement")
	}
}

func TestBlockFragmentCountsOnlyLivePlacementsAndNamesAStop(t *testing.T) {
	out := renderBlockFragment(t, &pb.Block{
		Metadata: &pb.Metadata{Namespace: "default", Name: "web"},
		Spec:     &pb.BlockSpec{Replicas: proto.Int32(1)},
		Status: &pb.BlockStatus{
			Phase:         pb.Phase_FAILED,
			PendingReason: &pb.PendingReason{Code: "LivenessFailed", Message: "replica 0 failed its liveness probe on 2 nodes and was stopped"},
			Placements: []*pb.PlacementStatus{
				{ReplicaIndex: -1, NodeId: "node-a", Phase: pb.Phase_FAILED},
				{ReplicaIndex: -1, NodeId: "node-b", Phase: pb.Phase_LOST},
			},
		},
	})
	if !strings.Contains(out, `<div class="kv-value">0</div><div class="kv-sub">replica slots`) {
		t.Error("Placed on counts stopped placements")
	}
	if !strings.Contains(out, "Replica stopped: LivenessFailed") || strings.Contains(out, "Not fully placed") {
		t.Error("callout does not say the replica was stopped")
	}
	if !strings.Contains(out, "node lost") {
		t.Error("a lost placement does not say its node was lost")
	}
}
