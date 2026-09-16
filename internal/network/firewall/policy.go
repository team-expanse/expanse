// §4.6 basic per-block network policy. Phase 05 keeps this simple and
// IP-keyed (full micro-segmentation is Phase 14): blocks on a node
// share the host network namespace, so the only workable key without
// cgroup/uid matching is the node's overlay address plus the block's
// declared service ports.
//
// Ingress is enforced: a block whose spec declares a policy gets, in
// the blockpol input chain, accept rules for its allowed sources
// followed by explicit drop rules for its own ports (default deny for
// that direction). Blocks without a policy contribute no rules —
// default allow (compatibility).
//
// Egress accept rules are compiled into blockpol-egress (output chain,
// policy accept) so the declared intent is visible and inspectable,
// but no node-wide drop is installed: all blocks (and the agent
// itself) share one output path, so a default-deny there would sever
// cluster traffic. Real egress isolation waits for cgroup keying.
package firewall

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// Policy chains: blockpol filters ingress (hook input, after the main
// chain); blockpolEgress records egress intent (hook output).
const (
	PolicyChain       = "blockpol"
	PolicyChainEgress = "blockpol-egress"
	// Runs after the main filter chain (priority 0).
	PolicyPriority     = 10
	OverlayAggregateVV = "10.42.0.0/16" // §3 aggregate; egress "external" = not this
)

// PolicyPort is one transport port in a policy rule.
type PolicyPort struct {
	Port     uint16
	Protocol string // "tcp" (default) or "udp"
}

// PolicyFrom names allowed ingress sources.
type PolicyFrom struct {
	Blocks []string       // block names, same namespace
	CIDRs  []netip.Prefix // IP ranges
}

// PolicyTo names allowed egress destinations.
type PolicyTo struct {
	External bool           // anything outside the overlay
	Blocks   []string       // block names, same namespace
	CIDRs    []netip.Prefix // IP ranges
}

// IngressRule is one accept entry of a block's ingress policy.
type IngressRule struct {
	From  PolicyFrom
	Ports []PolicyPort // empty = all the block's ports
}

// EgressRule is one accept entry of a block's egress policy.
type EgressRule struct {
	To    PolicyTo
	Ports []PolicyPort // empty = all the block's ports
}

// BlockPolicy is one block's compiled policy input: its key
// ("<ns>/<name>"), the overlay host address of the node(s) serving it,
// its declared service ports, and the parsed spec rules.
type BlockPolicy struct {
	Key     string
	Overlay netip.Addr // this node's overlay address (1042.N.1)
	Ports   []PolicyPort
	Ingress []IngressRule
	Egress  []EgressRule
}

// policiesKey is the canonical change-detection string: the ruleset
// only needs a rebuild when this changes.
func policiesKey(pols []BlockPolicy, peers map[string][]netip.Addr) string {
	key := func(p PolicyPort) string {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		return fmt.Sprintf("%s/%d", proto, p.Port)
	}
	var b strings.Builder
	sorted := append([]BlockPolicy(nil), pols...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	for _, pol := range sorted {
		fmt.Fprintf(&b, "block %s overlay %s ports [", pol.Key, pol.Overlay)
		ports := append([]PolicyPort(nil), pol.Ports...)
		sort.Slice(ports, func(i, j int) bool { return ports[i].Port < ports[j].Port })
		for _, p := range ports {
			b.WriteString(key(p) + " ")
		}
		b.WriteString("] ingress [")
		for _, r := range pol.Ingress {
			names := append([]string(nil), r.From.Blocks...)
			sort.Strings(names)
			cidrs := make([]string, 0, len(r.From.CIDRs))
			for _, c := range r.From.CIDRs {
				cidrs = append(cidrs, c.String())
			}
			sort.Strings(cidrs)
			fmt.Fprintf(&b, "from blocks%v cidrs%v ports[", names, cidrs)
			for _, p := range r.Ports {
				b.WriteString(key(p) + " ")
			}
			b.WriteString("] ")
		}
		b.WriteString("] egress [")
		for _, r := range pol.Egress {
			names := append([]string(nil), r.To.Blocks...)
			sort.Strings(names)
			cidrs := make([]string, 0, len(r.To.CIDRs))
			for _, c := range r.To.CIDRs {
				cidrs = append(cidrs, c.String())
			}
			sort.Strings(cidrs)
			fmt.Fprintf(&b, "to ext=%v blocks%v cidrs%v ports[", r.To.External, names, cidrs)
			for _, p := range r.Ports {
				b.WriteString(key(p) + " ")
			}
			b.WriteString("] ")
		}
		b.WriteString("]\n")
	}
	// Peer block overlay addresses feed from.blocks resolution.
	names := make([]string, 0, len(peers))
	for n := range peers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		addrs := append([]netip.Addr(nil), peers[n]...)
		sort.Slice(addrs, func(i, j int) bool { return addrs[i].Less(addrs[j]) })
		fmt.Fprintf(&b, "peer %s [", n)
		for _, a := range addrs {
			b.WriteString(a.String() + " ")
		}
		b.WriteString("]\n")
	}
	return b.String()
}

// policyExprs compiles one ingress accept entry: ip saddr <cidr> ip
// daddr <overlay> <proto> dport <port> accept.
func policyExprs(src netip.Prefix, dst netip.Addr, p PolicyPort) []expr.Any {
	proto := unix.IPPROTO_TCP
	if strings.EqualFold(p.Protocol, "udp") {
		proto = unix.IPPROTO_UDP
	}
	// ip saddr & <cidr mask> == <cidr net> (a /32 peer is the exact case).
	mask4 := maskWords(net.CIDRMask(src.Bits(), 32))
	net4 := src.Masked().Addr().As4()
	d := dst.As4()
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
		&expr.Bitwise{
			SourceRegister: 1, DestRegister: 1, Len: 4,
			Mask: binaryutil.BigEndian.PutUint32(mask4), Xor: binaryutil.BigEndian.PutUint32(0),
		},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: net4[:]},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: d[:]},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{byte(proto)}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(p.Port)},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

// maskWords converts an IPv4 netmask to a big-endian uint32 for the
// nftables Bitwise expression.
func maskWords(m net.IPMask) uint32 {
	return binary.BigEndian.Uint32(m)
}

func policyDropExprs(dst netip.Addr, p PolicyPort) []expr.Any {
	proto := unix.IPPROTO_TCP
	if strings.EqualFold(p.Protocol, "udp") {
		proto = unix.IPPROTO_UDP
	}
	d := dst.As4()
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: d[:]},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{byte(proto)}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(p.Port)},
		&expr.Verdict{Kind: expr.VerdictDrop},
	}
}

// egressExprs compiles one egress accept entry against dst CIDR (the
// overlay aggregate is inverted for to.external).
func egressExprs(dst netip.Prefix, p PolicyPort, invert bool) []expr.Any {
	proto := unix.IPPROTO_TCP
	if strings.EqualFold(p.Protocol, "udp") {
		proto = unix.IPPROTO_UDP
	}
	base := dst.Masked().Addr().As4()
	ones := dst.Bits()
	mask := net.CIDRMask(ones, 32)
	var exprs []expr.Any
	exprs = append(exprs,
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Bitwise{
			SourceRegister: 1, DestRegister: 1, Len: 4,
			Mask: binaryutil.BigEndian.PutUint32(maskWords(mask)), Xor: binaryutil.BigEndian.PutUint32(0),
		},
	)
	cmpOp := expr.CmpOpEq
	if invert {
		cmpOp = expr.CmpOpNeq
	}
	exprs = append(exprs, &expr.Cmp{Op: cmpOp, Register: 1, Data: base[:]})
	exprs = append(exprs,
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{byte(proto)}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(p.Port)},
		&expr.Verdict{Kind: expr.VerdictAccept},
	)
	return exprs
}

// dropPorts expands the drop set: the block's declared ports, or the
// union of rule ports when the spec's port list is empty.
func dropPorts(pol BlockPolicy) []PolicyPort {
	if len(pol.Ports) > 0 {
		return pol.Ports
	}
	seen := map[PolicyPort]bool{}
	var out []PolicyPort
	for _, r := range pol.Ingress {
		for _, p := range r.Ports {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// IngressRules compiles one block's ingress policy into ordered rules
// for the blockpol chain: accept entries first, then the default-deny
// drops for the block's own ports. peerBlocks resolves from.blocks
// names to the overlay addresses their replicas live on.
func IngressRules(pol BlockPolicy, peerBlocks map[string][]netip.Addr) []*nftables.Rule {
	t := &nftables.Table{Family: nftables.TableFamilyINet, Name: Table}
	ch := &nftables.Chain{Table: t, Name: PolicyChain}
	var rs []*nftables.Rule
	add := func(exprs []expr.Any) {
		rs = append(rs, &nftables.Rule{Table: t, Chain: ch, Exprs: exprs})
	}
	for _, r := range pol.Ingress {
		var srcs []netip.Prefix
		for _, name := range r.From.Blocks {
			for _, a := range peerBlocks[name] {
				srcs = append(srcs, netip.PrefixFrom(a, 32))
			}
		}
		srcs = append(srcs, r.From.CIDRs...)
		ports := r.Ports
		if len(ports) == 0 {
			ports = dropPorts(pol)
		}
		for _, src := range srcs {
			for _, p := range ports {
				add(policyExprs(src, pol.Overlay, p))
			}
		}
	}
	// Default deny only when the direction actually has rules (a
	// policy block without ingress entries stays default-allow).
	if len(pol.Ingress) > 0 {
		for _, p := range dropPorts(pol) {
			add(policyDropExprs(pol.Overlay, p))
		}
	}
	return rs
}

// EgressRules compiles one block's egress accept entries. Destinations
// outside the overlay are expressed as "not 10.42.0.0/16"; the output
// chain's policy stays accept (see package comment).
func EgressRules(pol BlockPolicy, peerBlocks map[string][]netip.Addr) []*nftables.Rule {
	t := &nftables.Table{Family: nftables.TableFamilyINet, Name: Table}
	ch := &nftables.Chain{Table: t, Name: PolicyChainEgress}
	overlay := netip.MustParsePrefix(OverlayAggregateVV)
	var rs []*nftables.Rule
	add := func(exprs []expr.Any) {
		rs = append(rs, &nftables.Rule{Table: t, Chain: ch, Exprs: exprs})
	}
	for _, r := range pol.Egress {
		ports := r.Ports
		if len(ports) == 0 {
			ports = dropPorts(pol)
		}
		for _, name := range r.To.Blocks {
			for _, a := range peerBlocks[name] {
				for _, p := range ports {
					add(egressExprs(netip.PrefixFrom(a, 32), p, false))
				}
			}
		}
		for _, c := range r.To.CIDRs {
			for _, p := range ports {
				add(egressExprs(c, p, false))
			}
		}
		if r.To.External {
			for _, p := range ports {
				add(egressExprs(overlay, p, true))
			}
		}
	}
	return rs
}

// BootstrapPolicies creates the two (empty) policy chains. Part of the
// one-time bootstrap; safe to call only before/with the main Flush.
func BootstrapPolicies(c Conn, t *nftables.Table) error {
	for _, name := range []string{PolicyChain, PolicyChainEgress} {
		hook := nftables.ChainHookInput
		if name == PolicyChainEgress {
			hook = nftables.ChainHookOutput
		}
		c.AddChain(&nftables.Chain{
			Table:    t,
			Name:     name,
			Type:     nftables.ChainTypeFilter,
			Hooknum:  hook,
			Priority: nftables.ChainPriorityRef(PolicyPriority),
			Policy:   ptr(nftables.ChainPolicyAccept),
		})
	}
	return nil
}

// SyncPolicies maintains the policy chains. prev is the caller's
// (agent's) cached policiesKey; when the compiled key is unchanged the
// call is a no-op (no kernel traffic at all). On change both chains
// are flushed and refilled — policy edits are user-driven and rare,
// and the flush is scoped to these two chains, so the D5.6 no-reload
// invariant still holds for everything else.
func SyncPolicies(c Conn, pols []BlockPolicy, peers map[string][]netip.Addr, prev string) (string, error) {
	k := policiesKey(pols, peers)
	if k == prev {
		return k, nil
	}
	t := &nftables.Table{Family: nftables.TableFamilyINet, Name: Table}
	in := &nftables.Chain{Table: t, Name: PolicyChain}
	out := &nftables.Chain{Table: t, Name: PolicyChainEgress}
	c.FlushChain(in)
	c.FlushChain(out)
	for _, pol := range pols {
		for _, r := range IngressRules(pol, peers) {
			c.AddRule(r)
		}
		for _, r := range EgressRules(pol, peers) {
			c.AddRule(r)
		}
	}
	if err := c.Flush(); err != nil {
		return prev, fmt.Errorf("firewall: policy flush: %w", err)
	}
	return k, nil
}
