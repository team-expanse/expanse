package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

func TestBlockStatusTableShowsReplicaHealthByNode(t *testing.T) {
	now := time.Unix(1700000100, 0)
	probed := time.Unix(1700000088, 0).UnixNano()
	b := &pb.Block{
		Metadata: &pb.Metadata{Namespace: "default", Name: "web"},
		Spec:     &pb.BlockSpec{Type: "web/nginx", Replicas: proto.Int32(2)},
		Status: &pb.BlockStatus{
			Phase:    pb.Phase_DEGRADED,
			Replicas: &pb.StatusReplicas{Ready: 1},
			PendingReason: &pb.PendingReason{
				Code: "LivenessFailed", Message: "replica 1 failed its liveness probe on 2 nodes and was stopped",
				PerNode: map[string]string{"node-c": "replica failed its liveness probe here"},
			},
			Placements: []*pb.PlacementStatus{
				{ReplicaIndex: 0, NodeId: "node-a", Phase: pb.Phase_RUNNING, Health: &pb.ReplicaHealth{
					Readiness: &pb.ProbeResult{Ok: true, Detail: "200 OK", AtUnixNs: probed}, Restarts: 1,
					Liveness: &pb.ProbeResult{Ok: true, Detail: "timeout", AtUnixNs: probed},
				}},
				{ReplicaIndex: -1, FormerIndex: 1, NodeId: "node-c", Phase: pb.Phase_FAILED,
					Message: "liveness probe failed after 5 restarts: refused"},
			},
		},
	}
	var buf bytes.Buffer
	printBlockStatus(&buf, b, now)
	out := buf.String()
	for _, want := range []string{
		"default/web", "web/nginx", "DEGRADED", "1/2 ready",
		"LivenessFailed: replica 1 failed its liveness probe on 2 nodes and was stopped",
		"node-c: replica failed its liveness probe here",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	rows := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 1 && strings.HasPrefix(f[1], "node-") {
			rows[f[1]] = strings.Join(f, " ")
		}
	}
	if want := "0 node-a RUNNING yes 1 200 OK (12s ago)"; rows["node-a"] != want {
		t.Errorf("node-a row = %q, want %q", rows["node-a"], want)
	}
	if want := "1 node-c FAILED - - stopped: liveness probe failed after 5 restarts: refused"; rows["node-c"] != want {
		t.Errorf("node-c row = %q, want %q", rows["node-c"], want)
	}
}

func TestBlockStatusTableMarksUnknownHealth(t *testing.T) {
	b := &pb.Block{
		Metadata: &pb.Metadata{Namespace: "default", Name: "web"},
		Spec:     &pb.BlockSpec{Replicas: proto.Int32(1)},
		Status: &pb.BlockStatus{Phase: pb.Phase_STARTING, Placements: []*pb.PlacementStatus{
			{ReplicaIndex: 0, NodeId: "node-a", Phase: pb.Phase_STARTING},
		}},
	}
	var buf bytes.Buffer
	printBlockStatus(&buf, b, time.Now())
	if !strings.Contains(buf.String(), "0        node-a  STARTING  ?      0") {
		t.Errorf("unknown health row wrong:\n%s", buf.String())
	}
}
