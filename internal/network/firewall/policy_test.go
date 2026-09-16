package firewall

import (
	"net/netip"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

var (
	ovlN1 = netip.MustParseAddr("10.42.1.1")
	ovlN2 = netip.MustParseAddr("10.42.2.1")
	ovlN3 = netip.MustParseAddr("10.42.3.1")
	peers = map[string][]netip.Addr{
		"web":  {ovlN2},
		"api":  {ovlN2, ovlN3},
		"self": {ovlN1},
	}
	tcp5432  = PolicyPort{Port: 5432, Protocol: "tcp"}
	udp5353  = PolicyPort{Port: 5353, Protocol: "udp"}
	postgres = BlockPolicy{
		Key:     "default/db",
		Overlay: ovlN1,
		Ports:   []PolicyPort{tcp5432},
		Ingress: []IngressRule{{From: PolicyFrom{Blocks: []string{"web"}}, Ports: []PolicyPort{tcp5432}}, {From: PolicyFrom{CIDRs: []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}}}},
		Egress:  []EgressRule{{To: PolicyTo{External: true}}},
	}
)

func cmpData(r *nftables.Rule, i int) []byte {
	if i < len(r.Exprs) {
		if c, ok := r.Exprs[i].(*expr.Cmp); ok {
			return c.Data
		}
	}
	return nil
}

func verdict(r *nftables.Rule, i int) expr.VerdictKind {
	if i < len(r.Exprs) {
		if v, ok := r.Exprs[i].(*expr.Verdict); ok {
			return v.Kind
		}
	}
	return -1
}

func isVerdict(r *nftables.Rule, k expr.VerdictKind) bool {
	for _, e := range r.Exprs {
		if v, ok := e.(*expr.Verdict); ok && v.Kind == k {
			return true
		}
	}
	return false
}

// Absent policy: no rules at all (default allow, §4.6).
func TestPolicyAbsentAllowsAll(t *testing.T) {
	if rs := IngressRules(BlockPolicy{Key: "x", Overlay: ovlN1, Ports: []PolicyPort{tcp5432}}, peers); len(rs) != 0 {
		t.Fatalf("absent ingress policy produced %d rules, want 0", len(rs))
	}
	if rs := EgressRules(BlockPolicy{Key: "x", Overlay: ovlN1}, peers); len(rs) != 0 {
		t.Fatalf("absent egress policy produced %d rules, want 0", len(rs))
	}
}

// §4.6 YAML shape 1: from.blocks + ports → accept from the peer
// block's overlay address on the declared port, then a default-deny
// drop for the same daddr+port.
func TestIngressFromBlocks(t *testing.T) {
	pol := BlockPolicy{
		Key:     "default/db",
		Overlay: ovlN1,
		Ports:   []PolicyPort{tcp5432},
		Ingress: []IngressRule{{From: PolicyFrom{Blocks: []string{"web"}}, Ports: []PolicyPort{tcp5432}}},
	}
	rs := IngressRules(pol, peers)
	if len(rs) != 2 {
		t.Fatalf("got %d rules, want 2 (accept + drop)", len(rs))
	}
	if !isVerdict(rs[0], expr.VerdictAccept) || !isVerdict(rs[1], expr.VerdictDrop) {
		t.Fatalf("want accept-then-drop, got %v then %v", verdict(rs[0], 0), verdict(rs[1], 0))
	}
	// Accept: saddr cmp = 10.42.2.1 masked network; daddr = 10.42.1.1;
	// dport = 5432.
	if got := cmpData(rs[0], 2); got[0] != ovlN2.As4()[0] || got[1] != ovlN2.As4()[1] || got[2] != ovlN2.As4()[2] || got[3] != ovlN2.As4()[3] {
		t.Fatalf("saddr cmp = %v, want 10.42.2.1", got)
	}
	if got := cmpData(rs[0], 4); got[0] != ovlN1.As4()[0] || got[1] != ovlN1.As4()[1] || got[2] != ovlN1.As4()[2] || got[3] != ovlN1.As4()[3] {
		t.Fatalf("daddr cmp = %v, want 10.42.1.1", got)
	}
	if got := cmpData(rs[0], 8); string(got) != "\x15\x38" { // 5432 BE
		t.Fatalf("dport cmp = %v, want 5432", got)
	}
}

// §4.6 YAML shape 2: from.cidrs (rule without ports = all block ports).
func TestIngressFromCIDRs(t *testing.T) {
	pol := BlockPolicy{
		Key:     "default/db",
		Overlay: ovlN1,
		Ports:   []PolicyPort{tcp5432, udp5353},
		Ingress: []IngressRule{{From: PolicyFrom{CIDRs: []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}}}},
	}
	rs := IngressRules(pol, peers)
	// 1 CIDR × 2 ports accepts + 2 port drops.
	if len(rs) != 4 {
		t.Fatalf("got %d rules, want 4", len(rs))
	}
	// The accept's saddr cmp carries the masked network.
	if got := cmpData(rs[0], 2); string(got) != "\xc0\xa8\x01\x00" {
		t.Fatalf("saddr cmp = %v, want 192.168.1.0", got)
	}
}

// §4.6 YAML shape 3: egress to.external — an inverted (not-overlay)
// accept in the egress chain.
func TestEgressExternal(t *testing.T) {
	pol := BlockPolicy{Key: "default/db", Overlay: ovlN1, Ports: []PolicyPort{tcp5432}, Egress: postgres.Egress}
	rs := EgressRules(pol, peers)
	if len(rs) != 1 || !isVerdict(rs[0], expr.VerdictAccept) {
		t.Fatalf("want 1 accept rule, got %d", len(rs))
	}
	// Daddr payload compared NEQ the overlay aggregate base.
	for _, e := range rs[0].Exprs {
		if c, ok := e.(*expr.Cmp); ok && c.Op == expr.CmpOpNeq {
			if string(c.Data) != "\x0a\x2a\x00\x00" {
				t.Fatalf("external cmp = %v, want 10.42.0.0", c.Data)
			}
			return
		}
	}
	t.Fatal("no NEQ daddr comparison found")
}

// Egress to.blocks/cidrs compile to plain accepts.
func TestEgressBlocksAndCIDRs(t *testing.T) {
	pol := BlockPolicy{
		Key:     "default/db",
		Overlay: ovlN1,
		Ports:   []PolicyPort{tcp5432},
		Egress: []EgressRule{
			{To: PolicyTo{Blocks: []string{"api"}, CIDRs: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}}},
		},
	}
	rs := EgressRules(pol, peers)
	// api has 2 overlay addrs + 1 cidr = 3 accepts.
	if len(rs) != 3 {
		t.Fatalf("got %d rules, want 3", len(rs))
	}
	for _, r := range rs {
		if !isVerdict(r, expr.VerdictAccept) {
			t.Fatal("want accept verdict")
		}
	}
}

// Default deny covers the block's ports even when a rule omits ports.
func TestDefaultDenyCoversBlockPorts(t *testing.T) {
	pol := BlockPolicy{
		Key:     "default/db",
		Overlay: ovlN1,
		Ports:   []PolicyPort{tcp5432, udp5353},
		Ingress: []IngressRule{{From: PolicyFrom{Blocks: []string{"web"}}}}, // no ports
	}
	rs := IngressRules(pol, peers)
	// accepts: 1 peer × 2 ports; drops: 2 ports.
	drops := 0
	for _, r := range rs {
		if isVerdict(r, expr.VerdictDrop) {
			drops++
		}
	}
	if drops != 2 {
		t.Fatalf("got %d drop rules, want 2 (one per block port)", drops)
	}
}

// SyncPolicies: no-op when the compiled key is unchanged; flush +
// refill when it changes; error passthrough from the conn.
func TestSyncPoliciesNoopAndChange(t *testing.T) {
	f := newFakeConn()
	pols := []BlockPolicy{postgres}
	k, err := SyncPolicies(f, pols, peers, "")
	if err != nil {
		t.Fatal(err)
	}
	flushes := 0
	for _, op := range f.ops {
		if len(op) > 10 && op[:10] == "flushChain" {
			flushes++
		}
	}
	if flushes != 2 {
		t.Fatalf("first sync did %d chain flushes, want 2", flushes)
	}
	f.ops = nil
	k2, err := SyncPolicies(f, pols, peers, k)
	if err != nil || k2 != k {
		t.Fatalf("unchanged sync: key %q != %q (err %v)", k2, k, err)
	}
	if len(f.ops) != 0 {
		t.Fatalf("unchanged sync issued %d ops, want 0: %v", len(f.ops), f.ops)
	}
	// A spec change flips the key and re-flushes.
	pols[0].Ports = append(pols[0].Ports, udp5353)
	if _, err := SyncPolicies(f, pols, peers, k); err != nil {
		t.Fatal(err)
	}
	if len(f.ops) == 0 {
		t.Fatal("changed policy produced no ops")
	}
}

func TestSyncPoliciesErrorPropagates(t *testing.T) {
	f := newFakeConn()
	f.err = errInjected
	if _, err := SyncPolicies(f, []BlockPolicy{postgres}, peers, ""); err == nil {
		t.Fatal("want error from failed flush")
	}
}

var errInjected = &errTest{}

type errTest struct{}

func (*errTest) Error() string { return "injected" }

// Compile-time: policies chain bootstrap adds the two chains.
func TestBootstrapPoliciesChains(t *testing.T) {
	f := newFakeConn()
	if err := BootstrapPolicies(f, &nftables.Table{Family: nftables.TableFamilyINet, Name: Table}); err != nil {
		t.Fatal(err)
	}
	adds := 0
	for _, op := range f.ops {
		if op == "addChain" {
			adds++
		}
	}
	if adds != 2 {
		t.Fatalf("bootstrap policies added %d chains, want 2", adds)
	}
}
