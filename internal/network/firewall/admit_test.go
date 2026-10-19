package firewall

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

// The admit chain runs ahead of the NixOS firewall's input chain and never drops.
func TestBootstrapAdmitChainPrecedesHostFirewall(t *testing.T) {
	f := newFakeConn()
	if err := BootstrapAdmit(f); err != nil {
		t.Fatalf("BootstrapAdmit: %v", err)
	}
	if len(f.chainDefs) != 1 {
		t.Fatalf("want 1 chain, got %d", len(f.chainDefs))
	}
	ch := f.chainDefs[0]
	if ch.Table.Name != AdmitTable || ch.Hooknum != nftables.ChainHookInput {
		t.Fatalf("chain %s in table %s on hook %v", ch.Name, ch.Table.Name, ch.Hooknum)
	}
	if *ch.Priority >= *nftables.ChainPriorityFilter {
		t.Fatalf("priority %d does not precede the host firewall's filter chain", *ch.Priority)
	}
	if ch.Policy == nil || *ch.Policy != nftables.ChainPolicyAccept {
		t.Fatal("the admit chain must not drop anything")
	}
	if want := []string{SetVIPs, SetTCPPorts, SetUDPPorts}; !slices.Equal(f.addedSets, want) {
		t.Fatalf("sets %v, want %v", f.addedSets, want)
	}
}

// Each rule marks a VIP-addressed packet on a declared port, and nothing else.
func TestBootstrapAdmitRulesOnlyMark(t *testing.T) {
	f := newFakeConn()
	if err := BootstrapAdmit(f); err != nil {
		t.Fatal(err)
	}
	if len(f.ruleDefs) != 2 {
		t.Fatalf("want a tcp and a udp rule, got %d", len(f.ruleDefs))
	}
	for i, r := range f.ruleDefs {
		var lookups []string
		marks := 0
		for _, e := range r.Exprs {
			switch e := e.(type) {
			case *expr.Lookup:
				lookups = append(lookups, e.SetName)
			case *expr.Meta:
				if e.Key == expr.MetaKeyMARK && e.SourceRegister {
					marks++
				}
			case *expr.Verdict:
				t.Fatalf("rule %d has a verdict; the host firewall decides", i)
			}
		}
		if !slices.Contains(lookups, SetVIPs) || len(lookups) != 2 || marks != 1 {
			t.Fatalf("rule %d: lookups %v, mark sets %d", i, lookups, marks)
		}
	}
}

// A restarted agent rebuilds its rules in place instead of stacking a second copy.
func TestBootstrapAdmitFlushesItsChainFirst(t *testing.T) {
	f := newFakeConn()
	if err := BootstrapAdmit(f); err != nil {
		t.Fatal(err)
	}
	flush := slices.Index(f.ops, "flushChain "+AdmitChain)
	rule := slices.Index(f.ops, "addRule")
	if flush < 0 || flush > rule {
		t.Fatalf("chain not flushed before its rules: %v", f.ops)
	}
}

func TestSyncAdmitTracksVIPsAndPorts(t *testing.T) {
	f := newFakeConn()
	if err := BootstrapAdmit(f); err != nil {
		t.Fatal(err)
	}
	f.ops = nil
	d := Desired{
		Peers:    []netip.Addr{netip.MustParseAddr("10.42.1.1")},
		VIPs:     []netip.Prefix{netip.MustParsePrefix("192.168.10.202/24")},
		TCPPorts: []uint16{80},
	}
	if err := SyncAdmit(f, d); err != nil {
		t.Fatal(err)
	}
	want := []string{"addElem vip_addresses 192.168.10.202", "addElem block_tcp_ports 80", "flush"}
	if !slices.Equal(f.ops, want) {
		t.Fatalf("ops %v, want %v (peers are not the admit table's business)", f.ops, want)
	}
	f.ops = nil
	if err := SyncAdmit(f, Desired{}); err != nil {
		t.Fatal(err)
	}
	want = []string{"delElem vip_addresses 192.168.10.202", "delElem block_tcp_ports 80", "flush"}
	if !slices.Equal(f.ops, want) {
		t.Fatalf("ops %v, want %v", f.ops, want)
	}
}
