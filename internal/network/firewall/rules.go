// Package firewall renders and applies the §4.5 nftables ruleset
// (table inet expanse). The static skeleton is created once at agent
// bootstrap; afterwards only set elements are added/deleted (D5.6) —
// full reloads are never issued, because they drop conntrack state and
// break live connections.
package firewall

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// TableName / ChainName identify the generated ruleset.
const (
	Table       = "expanse"
	Chain       = "input"
	SetPeers    = "cluster_peers"
	SetVIPs     = "vip_addresses"
	SetTCPPorts = "block_tcp_ports"
	SetUDPPorts = "block_udp_ports"
)

// Cluster-service and management ports from §4.5.
const (
	PortMesh    = 51820 // wireguard (udp, peers only)
	PortCA      = 7443  // tcp, overlay only
	PortRaft    = 7444  // tcp, overlay only
	PortJoin    = 7445  // tcp + udp, overlay only
	PortMgmt    = 7446  // tcp, overlay only
	PortSSH     = 22    // tcp, any
	PortWebUI   = 8443  // tcp, any
	PortMDNS    = 5353  // udp, any
	PortDNSNode = 53    // udp, overlay only
)

// Conn is the subset of *nftables.Conn the package uses. It exists so
// tests can record operations without a kernel.
type Conn interface {
	AddTable(*nftables.Table) *nftables.Table
	AddChain(*nftables.Chain) *nftables.Chain
	AddRule(*nftables.Rule) *nftables.Rule
	AddSet(*nftables.Set, []nftables.SetElement) error
	GetSetElements(*nftables.Set) ([]nftables.SetElement, error)
	SetAddElements(*nftables.Set, []nftables.SetElement) error
	SetDeleteElements(*nftables.Set, []nftables.SetElement) error
	Flush() error
}

// Static asserts *nftables.Conn implements Conn at compile time.
var _ Conn = (*nftables.Conn)(nil)

// Desired is the caller-friendly form of the four dynamic sets.
type Desired struct {
	Peers    []netip.Addr // cluster nodes' exp0 addresses (10.42.N.1)
	VIPs     []netip.Prefix
	TCPPorts []uint16
	UDPPorts []uint16
}

type desiredSets struct {
	peers map[string]bool // raw 4-byte IPv4
	vips  map[string]bool // prefix base address, 4 bytes
	tcp   map[string]bool // 2 bytes big-endian
	udp   map[string]bool
}

func (d Desired) normalize() desiredSets {
	enc := desiredSets{peers: map[string]bool{}, vips: map[string]bool{}, tcp: map[string]bool{}, udp: map[string]bool{}}
	for _, a := range d.Peers {
		if a.IsValid() && a.Is4() {
			b := a.As4()
			enc.peers[string(b[:])] = true
		}
	}
	for _, p := range d.VIPs {
		if !p.IsValid() || !p.Addr().Is4() {
			continue
		}
		b := p.Addr().As4()
		enc.vips[string(b[:])] = true
	}
	for _, p := range d.TCPPorts {
		enc.tcp[portKey(p)] = true
	}
	for _, p := range d.UDPPorts {
		enc.udp[portKey(p)] = true
	}
	return enc
}

func portKey(p uint16) string {
	return string(binaryutil.BigEndian.PutUint16(p))
}

func set4(t *nftables.Table, name string) *nftables.Set {
	return &nftables.Set{Table: t, Name: name, KeyType: nftables.TypeIPAddr, KeyByteOrder: binaryutil.BigEndian}
}

func set16(t *nftables.Table, name string) *nftables.Set {
	return &nftables.Set{Table: t, Name: name, KeyType: nftables.TypeInetService, KeyByteOrder: binaryutil.BigEndian}
}

// sets returns the four dynamic set definitions bound to the table.
func sets(t *nftables.Table) []*nftables.Set {
	return []*nftables.Set{set4(t, SetPeers), set4(t, SetVIPs), set16(t, SetTCPPorts), set16(t, SetUDPPorts)}
}

// Bootstrap creates the static ruleset skeleton (§4.5 verbatim):
// table/chain, static rules, the four dynamic sets, all in one atomic
// Flush. The caller runs it once at agent start and never again (D5.6).
func Bootstrap(c Conn) error {
	t := &nftables.Table{Family: nftables.TableFamilyINet, Name: Table}
	c.AddTable(t)
	ch := &nftables.Chain{
		Name:     Chain,
		Table:    t,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookInput,
		Priority: nftables.ChainPriorityFilter,
		Policy:   ptr(nftables.ChainPolicyDrop),
	}
	c.AddChain(ch)

	// Sets first: the kernel processes batch messages in order, and the
	// static rules' lookup expressions resolve their sets by name.
	for _, s := range sets(t) {
		if err := c.AddSet(s, nil); err != nil {
			return fmt.Errorf("firewall: add set %s: %w", s.Name, err)
		}
	}
	for _, r := range staticRules(t, ch) {
		c.AddRule(r)
	}
	return c.Flush()
}

func ptr[T any](v T) *T { return &v }

// staticRules builds the non-dynamic accept/drop rules in §4.5 order.
// The two dynamic-set rules reference the four sets created alongside.
func staticRules(t *nftables.Table, ch *nftables.Chain) []*nftables.Rule {
	proto := func(p byte) []expr.Any {
		return []expr.Any{
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{p}},
		}
	}
	dportEq := func(port uint16, p byte) []expr.Any {
		return append(proto(p), []expr.Any{
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(port)},
		}...)
	}
	dportSet := func(p byte, set string) []expr.Any {
		return append(proto(p), []expr.Any{
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			&expr.Lookup{SourceRegister: 1, SetName: set, Invert: false},
		}...)
	}
	saddrSet := func(set string) []expr.Any {
		return []expr.Any{
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
			&expr.Lookup{SourceRegister: 1, SetName: set, Invert: false},
		}
	}
	daddrSet := func(set string) []expr.Any {
		return []expr.Any{
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
			&expr.Lookup{SourceRegister: 1, SetName: set, Invert: false},
		}
	}
	iifname := func(name string) []expr.Any {
		return []expr.Any{
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(name)},
		}
	}

	var rs []*nftables.Rule
	add := func(exprs ...expr.Any) {
		rs = append(rs, &nftables.Rule{Table: t, Chain: ch, Exprs: exprs})
	}

	// ct state established,related accept
	add(
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{
			SourceRegister: 1, DestRegister: 1, Len: 4,
			Mask: binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED),
			Xor:  binaryutil.NativeEndian.PutUint32(0),
		},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0, 0, 0, 0}},
		&expr.Verdict{Kind: expr.VerdictAccept},
	)
	// ct state invalid drop
	add(
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{
			SourceRegister: 1, DestRegister: 1, Len: 4,
			Mask: binaryutil.NativeEndian.PutUint32(expr.CtStateBitINVALID),
			Xor:  binaryutil.NativeEndian.PutUint32(0),
		},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0, 0, 0, 0}},
		&expr.Verdict{Kind: expr.VerdictDrop},
	)
	// iif lo accept
	add(append(iifname("lo"), &expr.Verdict{Kind: expr.VerdictAccept})...)
	// ip protocol icmp accept
	add(append(proto(unix.IPPROTO_ICMP), &expr.Verdict{Kind: expr.VerdictAccept})...)
	// ip6 nexthdr icmpv6 accept
	add(append(proto(unix.IPPROTO_ICMPV6), &expr.Verdict{Kind: expr.VerdictAccept})...)

	// cluster mesh — only from known peers
	add(append(dportEq(PortMesh, unix.IPPROTO_UDP), append(saddrSet(SetPeers), &expr.Verdict{Kind: expr.VerdictAccept})...)...)

	// cluster services — from known peers on any interface (peers dial
	// each other's advertised endpoints, which may be LAN addresses;
	// §4.5's exp0-only rules below remain for overlay sources). Still
	// store-driven known peers only.
	for _, p := range []uint16{PortCA, PortRaft, PortJoin} {
		add(append(dportEq(p, unix.IPPROTO_TCP), append(saddrSet(SetPeers), &expr.Verdict{Kind: expr.VerdictAccept})...)...)
	}
	// 7446 is the join/control-plane endpoint: token- and mTLS-
	// authenticated, and by definition reachable to not-yet-peers. The
	// spec's off-overlay forbidden list (7443/7444/7445) omits it for
	// exactly this reason.
	add(append(dportEq(PortMgmt, unix.IPPROTO_TCP), &expr.Verdict{Kind: expr.VerdictAccept})...)

	// cluster services — only over the overlay (exp0)
	for _, p := range []uint16{PortCA, PortRaft, PortJoin, PortMgmt} {
		add(append(append(iifname("exp0"), dportEq(p, unix.IPPROTO_TCP)...), &expr.Verdict{Kind: expr.VerdictAccept})...)
	}
	for _, p := range []uint16{PortJoin, PortDNSNode} {
		add(append(append(iifname("exp0"), dportEq(p, unix.IPPROTO_UDP)...), &expr.Verdict{Kind: expr.VerdictAccept})...)
	}

	// management
	add(append(dportEq(PortSSH, unix.IPPROTO_TCP), &expr.Verdict{Kind: expr.VerdictAccept})...)
	add(append(dportEq(PortWebUI, unix.IPPROTO_TCP), &expr.Verdict{Kind: expr.VerdictAccept})...)
	add(append(dportEq(PortMDNS, unix.IPPROTO_UDP), &expr.Verdict{Kind: expr.VerdictAccept})...)

	// per-block declared ports, on VIP addresses only
	add(append(daddrSet(SetVIPs), append(dportSet(unix.IPPROTO_TCP, SetTCPPorts), &expr.Verdict{Kind: expr.VerdictAccept})...)...)
	add(append(daddrSet(SetVIPs), append(dportSet(unix.IPPROTO_UDP, SetUDPPorts), &expr.Verdict{Kind: expr.VerdictAccept})...)...)

	// counter log prefix "expanse-drop: " limit rate 10/minute
	// (no verdict: falls through to chain policy drop)
	add(&expr.Counter{}, &expr.Limit{Rate: 10, Unit: expr.LimitTimeMinute},
		&expr.Log{Key: unix.NFTA_LOG_PREFIX, Data: []byte("expanse-drop: ")})
	return rs
}

func ifname(s string) []byte {
	b := make([]byte, 16)
	copy(b, s)
	return b
}

// Render returns the §4.5 ruleset as nft(8) syntax, with the current
// dynamic set contents listed. Used by `expanse ctl firewall show`.
func Render(d Desired) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	w("table inet %s {", Table)
	w("  set %s { type ipv4_addr; elements = { %s } }", SetPeers, joinAddrs(d.Peers))
	w("  set %s { type ipv4_addr; elements = { %s } }", SetVIPs, joinAddrs(vipAddrs(d.VIPs)))
	w("  set %s { type inet_service; elements = { %s } }", SetTCPPorts, joinPorts(d.TCPPorts))
	w("  set %s { type inet_service; elements = { %s } }", SetUDPPorts, joinPorts(d.UDPPorts))
	w("  chain input {")
	w("    type filter hook input priority 0; policy drop;")
	w("    ct state established,related accept")
	w("    ct state invalid drop")
	w("    iif lo accept")
	w("    ip protocol icmp accept")
	w("    ip6 nexthdr icmpv6 accept")
	w("    # cluster mesh — only from known peers")
	w("    udp dport %d ip saddr @%s accept", PortMesh, SetPeers)
	w("    # cluster services — only over the overlay")
	w(`    iifname "exp0" tcp dport { %d, %d, %d, %d } accept`, PortCA, PortRaft, PortJoin, PortMgmt)
	w(`    iifname "exp0" udp dport { %d, %d } accept`, PortJoin, PortDNSNode)
	w("    # management")
	w("    tcp dport %d accept", PortSSH)
	w("    tcp dport %d accept", PortWebUI)
	w("    udp dport %d accept", PortMDNS)
	w("    # per-block declared ports, on VIP addresses only")
	w("    ip daddr @%s tcp dport @%s accept", SetVIPs, SetTCPPorts)
	w("    ip daddr @%s udp dport @%s accept", SetVIPs, SetUDPPorts)
	w("    counter log prefix \"expanse-drop: \" limit rate 10/minute")
	w("  }")
	w("}")
	return b.String()
}

func joinAddrs(addrs []netip.Addr) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a.IsValid() {
			parts = append(parts, a.String())
		}
	}
	return strings.Join(parts, ", ")
}

func vipAddrs(prefixes []netip.Prefix) []netip.Addr {
	out := make([]netip.Addr, 0, len(prefixes))
	for _, p := range prefixes {
		if p.IsValid() {
			out = append(out, p.Addr())
		}
	}
	return out
}

func joinPorts(ports []uint16) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, strconv.Itoa(int(p)))
	}
	return strings.Join(parts, ", ")
}
