package firewall

import (
	"fmt"
	"net/netip"
	"sort"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
)

// fakeConn records every operation in order, without a kernel.
type fakeConn struct {
	tables, chains, rules int
	addedSets             []string
	// live set contents, mutated only by element ops after Bootstrap.
	sets map[string]map[string]bool
	ops  []string // human-readable op log: "addElem block_tcp_ports 8080"
	err  error    // returned by element ops to test error paths
}

func newFakeConn() *fakeConn {
	return &fakeConn{sets: map[string]map[string]bool{
		SetPeers: {}, SetVIPs: {}, SetTCPPorts: {}, SetUDPPorts: {},
	}}
}

func (f *fakeConn) FlushChain(c *nftables.Chain) {
	f.ops = append(f.ops, "flushChain "+c.Name)
}

func (f *fakeConn) AddTable(*nftables.Table) *nftables.Table {
	f.tables++
	f.ops = append(f.ops, "addTable")
	return nil
}

func (f *fakeConn) AddChain(*nftables.Chain) *nftables.Chain {
	f.chains++
	f.ops = append(f.ops, "addChain")
	return nil
}

func (f *fakeConn) AddRule(*nftables.Rule) *nftables.Rule {
	f.rules++
	f.ops = append(f.ops, "addRule")
	return nil
}

func (f *fakeConn) AddSet(s *nftables.Set, _ []nftables.SetElement) error {
	f.addedSets = append(f.addedSets, s.Name)
	f.ops = append(f.ops, "addSet "+s.Name)
	return nil
}

func (f *fakeConn) GetSetElements(s *nftables.Set) ([]nftables.SetElement, error) {
	var out []nftables.SetElement
	keys := make([]string, 0, len(f.sets[s.Name]))
	for k := range f.sets[s.Name] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, nftables.SetElement{Key: []byte(k)})
	}
	return out, nil
}

func (f *fakeConn) SetAddElements(s *nftables.Set, vals []nftables.SetElement) error {
	if f.err != nil {
		return f.err
	}
	for _, v := range vals {
		f.sets[s.Name][string(v.Key)] = true
		f.ops = append(f.ops, fmt.Sprintf("addElem %s %s", s.Name, keyStr(v.Key)))
	}
	return nil
}

func (f *fakeConn) SetDeleteElements(s *nftables.Set, vals []nftables.SetElement) error {
	if f.err != nil {
		return f.err
	}
	for _, v := range vals {
		delete(f.sets[s.Name], string(v.Key))
		f.ops = append(f.ops, fmt.Sprintf("delElem %s %s", s.Name, keyStr(v.Key)))
	}
	return nil
}

func (f *fakeConn) Flush() error {
	f.ops = append(f.ops, "flush")
	return f.err
}

func keyStr(k []byte) string {
	switch len(k) {
	case 2:
		return fmt.Sprint(binaryutil.BigEndian.Uint16(k))
	case 4:
		return netip.AddrFrom4([4]byte(k)).String()
	}
	return fmt.Sprintf("%x", k)
}

func portSet(name string, ports ...uint16) map[string]bool {
	m := map[string]bool{}
	for _, p := range ports {
		m[portKey(p)] = true
	}
	return m
}

func TestBootstrapShape(t *testing.T) {
	f := newFakeConn()
	if err := Bootstrap(f); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if f.tables != 1 || f.chains != 1 {
		t.Fatalf("want 1 table + 1 chain, got %d/%d", f.tables, f.chains)
	}
	if len(f.addedSets) != 4 {
		t.Fatalf("want 4 dynamic sets, got %v", f.addedSets)
	}
	// §4.5 rule inventory: 2 ct + lo + icmp + icmpv6 + mesh + 4 tcp svc +
	// 2 udp svc + 3 mgmt + 2 vip + 4 peer-any-iface cluster rules + 1 log = 22.
	if f.rules != 22 {
		t.Fatalf("want 22 static rules, got %d", f.rules)
	}
}

func TestSyncDiffsOnly(t *testing.T) {
	f := newFakeConn()
	if err := Bootstrap(f); err != nil {
		t.Fatal(err)
	}
	f.ops = nil // stop recording bootstrap; D5.6 asserts nothing below re-adds structure

	base := Desired{
		Peers:    []netip.Addr{netip.MustParseAddr("10.42.1.1"), netip.MustParseAddr("10.42.2.1"), netip.MustParseAddr("10.42.3.1")},
		TCPPorts: []uint16{80},
	}
	if err := Sync(f, base); err != nil {
		t.Fatal(err)
	}
	for _, op := range f.ops {
		if op == "addTable" || op == "addChain" || op == "addRule" || op == "addSet" {
			t.Fatalf("post-bootstrap structure op: %s", op)
		}
	}

	// node add: exactly one new peer element
	f.ops = nil
	grown := Desired{Peers: append(append([]netip.Addr{}, base.Peers...), netip.MustParseAddr("10.42.4.1")), TCPPorts: []uint16{80}}
	if err := Sync(f, grown); err != nil {
		t.Fatal(err)
	}
	if len(f.ops) != 2 { // addElem + flush
		t.Fatalf("node-add ops = %v, want [addElem cluster_peers 10.42.4.1 flush]", f.ops)
	}

	// block scale 80→80,8080: one tcp port element
	f.ops = nil
	scaled := grown
	scaled.TCPPorts = []uint16{80, 8080}
	if err := Sync(f, scaled); err != nil {
		t.Fatal(err)
	}
	if len(f.ops) != 2 || f.ops[0] != "addElem block_tcp_ports 8080" {
		t.Fatalf("scale ops = %v, want addElem block_tcp_ports 8080 + flush", f.ops)
	}

	// node remove + port retraction: deletes only
	f.ops = nil
	shrunk := Desired{Peers: grown.Peers[:2], TCPPorts: []uint16{8080}}
	if err := Sync(f, shrunk); err != nil {
		t.Fatal(err)
	}
	var dels, adds int
	for _, op := range f.ops {
		switch {
		case op == "delElem cluster_peers 10.42.3.1", op == "delElem block_tcp_ports 80":
			dels++
		case op == "flush":
		default:
			if op[:7] == "addElem" {
				adds++
			}
		}
	}
	if dels != 2 || adds != 0 {
		t.Fatalf("shrink ops = %v (adds=%d dels=%d)", f.ops, adds, dels)
	}

	// idempotence: no ops at all when nothing changed
	f.ops = nil
	if err := Sync(f, shrunk); err != nil {
		t.Fatal(err)
	}
	if len(f.ops) != 1 { // only the flush
		t.Fatalf("idempotent sync issued ops: %v", f.ops)
	}

	// live contents match the last desired state exactly
	if len(f.sets[SetPeers]) != 2 || len(f.sets[SetTCPPorts]) != 1 {
		t.Fatalf("set contents wrong: %v %v", f.sets[SetPeers], f.sets[SetTCPPorts])
	}
}

func TestMembership(t *testing.T) {
	f := newFakeConn()
	_ = Bootstrap(f)
	_ = Sync(f, Desired{Peers: []netip.Addr{netip.MustParseAddr("10.42.1.1")}, TCPPorts: []uint16{80}, UDPPorts: []uint16{5353}})
	tcp, udp, vip, err := Membership(f, 80)
	if err != nil || !tcp || udp || vip {
		t.Fatalf("port 80: tcp=%t udp=%t vip=%t err=%v", tcp, udp, vip, err)
	}
	tcp, udp, vip, err = Membership(f, 9999)
	if err != nil || tcp || udp || vip {
		t.Fatalf("port 9999: tcp=%t udp=%t vip=%t err=%v", tcp, udp, vip, err)
	}
}

func TestRenderIsVerbatim(t *testing.T) {
	d := Desired{
		Peers:    []netip.Addr{netip.MustParseAddr("10.42.1.1")},
		VIPs:     []netip.Prefix{netip.MustParsePrefix("192.168.1.100/32")},
		TCPPorts: []uint16{80, 8080},
		UDPPorts: []uint16{53},
	}
	out := Render(d)
	for _, want := range []string{
		"table inet expanse {",
		"policy drop;",
		"ct state established,related accept",
		"udp dport 51820 ip saddr @cluster_peers accept",
		`iifname "exp0" tcp dport { 7443, 7444, 7445, 7446 } accept`,
		`iifname "exp0" udp dport { 7445, 53 } accept`,
		"tcp dport 22 accept",
		"tcp dport 8443 accept",
		"udp dport 5353 accept",
		"ip daddr @vip_addresses tcp dport @block_tcp_ports accept",
		"ip daddr @vip_addresses udp dport @block_udp_ports accept",
		`counter log prefix "expanse-drop: " limit rate 10/minute`,
		"elements = { 192.168.1.100 }",
	} {
		if !contains(out, want) {
			t.Errorf("render missing %q", want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}()
}

func TestSyncErrorPropagates(t *testing.T) {
	f := newFakeConn()
	f.err = fmt.Errorf("kernel said no")
	err := Sync(f, Desired{TCPPorts: []uint16{80}})
	if err == nil {
		t.Fatal("want error")
	}
}
