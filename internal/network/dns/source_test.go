package dns

import (
	"encoding/json"
	"testing"

	pb "github.com/expanse/expanse/proto"
	"github.com/miekg/dns"
	"google.golang.org/protobuf/proto"
)

// TestZoneSourceRebuild drives ingest/rebuild directly (the watch loop
// is store plumbing already covered by the proxy pool's equivalent).
func TestZoneSourceRebuild(t *testing.T) {
	s := NewServer()
	z := NewZoneSource(nil, s)

	blk, err := proto.Marshal(&pb.Block{Spec: &pb.BlockSpec{Network: &pb.Network{
		Ports: []*pb.Port{{Name: "http", Port: 8080, TargetPort: 80}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := proto.Marshal(&pb.BlockStatus{Placements: []*pb.PlacementStatus{
		{ReplicaIndex: 0, NodeId: "n1", Phase: pb.Phase_RUNNING},
		{ReplicaIndex: 1, NodeId: "n2", Phase: pb.Phase_RUNNING},
	}})
	if err != nil {
		t.Fatal(err)
	}
	vipAlloc, _ := json.Marshal(map[string]string{"addr": "192.168.1.100/24"})

	z.ingest("/blocks/default/web", blk)
	z.ingest("/blocks/default/web/status", st)
	z.ingest("/network/nodeIndexes/1", []byte("n1"))
	z.ingest("/network/nodeIndexes/2", []byte("n2"))
	z.ingest("/network/vipPool/internal/default/web", vipAlloc)
	z.rebuild()

	resp := s.Answer(query("web.default.expanse.local", dns.TypeA))
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 ||
		resp.Answer[0].(*dns.A).A.String() != "192.168.1.100" {
		t.Fatalf("VIP A: %+v", resp.Answer)
	}
	resp = s.Answer(query("1.web.default.expanse.local", dns.TypeA))
	if len(resp.Answer) != 1 || resp.Answer[0].(*dns.A).A.String() != "10.42.2.1" {
		t.Fatalf("replica A: %+v", resp.Answer)
	}
	resp = s.Answer(query("_http._tcp.web.default.expanse.local", dns.TypeSRV))
	if len(resp.Answer) != 2 {
		t.Fatalf("SRV per replica: %d", len(resp.Answer))
	}

	// Unhealthy replica: no SRV/replica record for index 1.
	z.ingest("/blocks/default/web/status/replicas/1", []byte(`{"ok":false}`))
	z.rebuild()
	resp = s.Answer(query("1.web.default.expanse.local", dns.TypeA))
	if len(resp.Answer) != 0 {
		t.Fatalf("unhealthy replica still published: %+v", resp.Answer)
	}

	// Delete the block: its records vanish, node records stay.
	z.remove("/blocks/default/web")
	z.rebuild()
	resp = s.Answer(query("web.default.expanse.local", dns.TypeA))
	if resp.Rcode != dns.RcodeNameError {
		t.Fatalf("block removal: %d", resp.Rcode)
	}
	resp = s.Answer(query("cluster.expanse.local", dns.TypeA))
	if len(resp.Answer) != 2 {
		t.Fatalf("cluster record after block removal: %d", len(resp.Answer))
	}
}
