package dns

import (
	"net/netip"
	"reflect"
	"sort"
	"testing"
)

// cmpRecord is the deterministic output order, shared with the wants
// below so they can be written in logical (not byte-sorted) order.
func cmpRecord(a, b Record) bool {
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	if a.Type != b.Type {
		return a.Type < b.Type
	}
	return a.Data < b.Data
}

func node(id string, a string) *NodeInfo {
	return &NodeInfo{ID: id, Addrs: []netip.Addr{netip.MustParseAddr(a)}}
}

func vipBlocks(input Input) []Record { return Build(input) }

func TestBuildNodeRecords(t *testing.T) {
	in := Input{Nodes: []NodeInfo{*node("n1", "10.42.1.1"), *node("n2", "10.42.2.1")}}
	got := Build(in)
	want := []Record{
		{Name: "cluster.expanse.local.", Type: "A", TTL: 300, Data: "10.42.1.1"},
		{Name: "cluster.expanse.local.", Type: "A", TTL: 300, Data: "10.42.2.1"},
		{Name: "n1.nodes.expanse.local.", Type: "A", TTL: 300, Data: "10.42.1.1"},
		{Name: "n2.nodes.expanse.local.", Type: "A", TTL: 300, Data: "10.42.2.1"},
	}
	sort.Slice(want, func(i, j int) bool { return cmpRecord(want[i], want[j]) })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestBuildBlockWithVIP(t *testing.T) {
	in := Input{
		Nodes: []NodeInfo{*node("n1", "10.42.1.1")},
		Blocks: []BlockInfo{{
			Namespace: "default", Name: "web",
			VIP: netip.MustParsePrefix("192.168.1.100/24"),
			Ports: []PortInfo{
				{Name: "http", Port: 8080, TargetPort: 80},
				{Name: "", Port: 9090}, // unnamed: no SRV
			},
			Replicas: []Replica{
				{Index: 0, Node: node("n1", "10.42.1.1"), Ready: true},
				{Index: 1, Node: node("n2", "10.42.2.1")}, // not RUNNING → not ready
			},
		}},
	}
	got := Build(in)
	want := []Record{
		{Name: "cluster.expanse.local.", Type: "A", TTL: 300, Data: "10.42.1.1"},
		{Name: "n1.nodes.expanse.local.", Type: "A", TTL: 300, Data: "10.42.1.1"},
		{Name: "web.default.expanse.local.", Type: "A", TTL: 5, Data: "192.168.1.100"},
		{Name: "0.web.default.expanse.local.", Type: "A", TTL: 5, Data: "10.42.1.1"},
		{Name: "_http._tcp.web.default.expanse.local.", Type: "SRV", TTL: 5, Data: "0 0 80 0.web.default.expanse.local."},
	}
	sort.Slice(want, func(i, j int) bool { return cmpRecord(want[i], want[j]) })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestBuildBlockWithoutVIP(t *testing.T) {
	// No VIP: the block A record lists every ready replica's node
	// overlay IP, deduplicated (two replicas on one node = one A).
	in := Input{
		Nodes: []NodeInfo{*node("n1", "10.42.1.1"), *node("n2", "10.42.2.1")},
		Blocks: []BlockInfo{{
			Namespace: "db", Name: "pg",
			Ports: []PortInfo{{Name: "psql", Port: 5432}},
			Replicas: []Replica{
				{Index: 1, Node: node("n2", "10.42.2.1"), Ready: true},
				{Index: 0, Node: node("n1", "10.42.1.1"), Ready: true},
				{Index: 2, Node: node("n1", "10.42.1.1"), Ready: true}, // same node as 0
			},
		}},
	}
	got := vipBlocks(in)
	want := []Record{
		{Name: "cluster.expanse.local.", Type: "A", TTL: 300, Data: "10.42.1.1"},
		{Name: "cluster.expanse.local.", Type: "A", TTL: 300, Data: "10.42.2.1"},
		{Name: "n1.nodes.expanse.local.", Type: "A", TTL: 300, Data: "10.42.1.1"},
		{Name: "n2.nodes.expanse.local.", Type: "A", TTL: 300, Data: "10.42.2.1"},
		{Name: "pg.db.expanse.local.", Type: "A", TTL: 5, Data: "10.42.1.1"},
		{Name: "pg.db.expanse.local.", Type: "A", TTL: 5, Data: "10.42.2.1"},
		{Name: "0.pg.db.expanse.local.", Type: "A", TTL: 5, Data: "10.42.1.1"},
		{Name: "1.pg.db.expanse.local.", Type: "A", TTL: 5, Data: "10.42.2.1"},
		{Name: "2.pg.db.expanse.local.", Type: "A", TTL: 5, Data: "10.42.1.1"},
		{Name: "_psql._tcp.pg.db.expanse.local.", Type: "SRV", TTL: 5, Data: "0 0 5432 0.pg.db.expanse.local."},
		{Name: "_psql._tcp.pg.db.expanse.local.", Type: "SRV", TTL: 5, Data: "0 0 5432 1.pg.db.expanse.local."},
		{Name: "_psql._tcp.pg.db.expanse.local.", Type: "SRV", TTL: 5, Data: "0 0 5432 2.pg.db.expanse.local."},
	}
	sort.Slice(want, func(i, j int) bool { return cmpRecord(want[i], want[j]) })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestBuildZeroReplicasProducesNoRecords(t *testing.T) {
	// Daemonset/singleton edge case: 0 ready replicas → no A record,
	// no SRV, no replica records, and not an error. Node records stay.
	in := Input{
		Nodes: []NodeInfo{*node("n1", "10.42.1.1")},
		Blocks: []BlockInfo{{
			Namespace: "default", Name: "gone",
			VIP:      netip.MustParsePrefix("192.168.1.100/24"),
			Ports:    []PortInfo{{Name: "http", Port: 80}},
			Replicas: []Replica{{Index: 0, Node: node("n1", "10.42.1.1"), Ready: false}},
		}},
	}
	got := Build(in)
	want := []Record{
		{Name: "cluster.expanse.local.", Type: "A", TTL: 300, Data: "10.42.1.1"},
		{Name: "n1.nodes.expanse.local.", Type: "A", TTL: 300, Data: "10.42.1.1"},
	}
	sort.Slice(want, func(i, j int) bool { return cmpRecord(want[i], want[j]) })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestBuildEmptyAndMalformed(t *testing.T) {
	if got := Build(Input{}); len(got) != 0 {
		t.Fatalf("empty input: got %+v", got)
	}
	// Malformed entries are skipped, not errors.
	got := Build(Input{
		Nodes:  []NodeInfo{{ID: ""}},       // no ID
		Blocks: []BlockInfo{{Name: "web"}}, // no namespace
	})
	_ = got
}

func TestReplicaPortDefaulting(t *testing.T) {
	p := PortInfo{Port: 8080}
	if p.ReplicaPort() != 8080 {
		t.Fatalf("target 0 must default to the service port")
	}
	p.TargetPort = 80
	if p.ReplicaPort() != 80 {
		t.Fatalf("explicit target must win")
	}
}
