package firewall

import (
	"fmt"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// The admit table marks packets for a block's VIP on its declared ports; the node's NixOS firewall
// accepts that mark (nix/modules/agent.nix), so it can stay default-deny with no per-block ports.
const (
	AdmitTable = "expanse-admit"
	AdmitChain = "mark"
	AdmitMark  = 0x02000000 // clear of kube-proxy's 0xc000 and tailscale's 0xff0000
)

func admitTable() *nftables.Table {
	return &nftables.Table{Family: nftables.TableFamilyINet, Name: AdmitTable}
}

func admitSets(t *nftables.Table) []*nftables.Set {
	return []*nftables.Set{set4(t, SetVIPs), set16(t, SetTCPPorts), set16(t, SetUDPPorts)}
}

// BootstrapAdmit creates or rebuilds the admit table in one batch; run at agent start and whenever
// the table has gone missing. Set contents survive; SyncAdmit maintains them.
func BootstrapAdmit(c Conn) error {
	t := admitTable()
	c.AddTable(t)
	ch := c.AddChain(&nftables.Chain{
		Name:     AdmitChain,
		Table:    t,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookInput,
		Priority: nftables.ChainPriorityRef(*nftables.ChainPriorityFilter - 10),
		Policy:   ptr(nftables.ChainPolicyAccept),
	})
	for _, s := range admitSets(t) {
		if err := c.AddSet(s, nil); err != nil {
			return fmt.Errorf("firewall: add set %s: %w", s.Name, err)
		}
	}
	c.FlushChain(ch)
	for _, p := range []struct {
		proto byte
		ports string
	}{{unix.IPPROTO_TCP, SetTCPPorts}, {unix.IPPROTO_UDP, SetUDPPorts}} {
		c.AddRule(&nftables.Rule{Table: t, Chain: ch, Exprs: admitRule(p.proto, p.ports)})
	}
	return c.Flush()
}

// admitRule: meta nfproto ipv4 meta l4proto <proto> ip daddr @vips th dport @<ports> meta mark |= AdmitMark.
func admitRule(proto byte, ports string) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.NFPROTO_IPV4}},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Lookup{SourceRegister: 1, SetName: SetVIPs},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Lookup{SourceRegister: 1, SetName: ports},
		&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
		&expr.Bitwise{
			SourceRegister: 1, DestRegister: 1, Len: 4,
			Mask: binaryutil.NativeEndian.PutUint32(^uint32(AdmitMark)),
			Xor:  binaryutil.NativeEndian.PutUint32(AdmitMark),
		},
		&expr.Meta{Key: expr.MetaKeyMARK, SourceRegister: true, Register: 1},
	}
}

// SyncAdmit moves the admit table's sets to d's VIPs and block ports (element diffs only).
func SyncAdmit(c Conn, d Desired) error {
	want := d.normalize()
	s := admitSets(admitTable())
	return syncSets(c, []setPlan{{s[0], want.vips}, {s[1], want.tcp}, {s[2], want.udp}})
}
