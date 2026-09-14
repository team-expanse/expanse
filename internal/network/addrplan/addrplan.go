// Package addrplan holds the Phase 05 address plan (PHASE05.md §3) as code,
// so every consumer (mesh, VIP, DNS, firewall, tests) derives addresses from
// one place instead of hard-coding literals.
package addrplan

import (
	"fmt"
	"net/netip"
)

// Address plan (PHASE05.md §3):
//
//	10.42.0.0/16  WireGuard overlay. Node N gets 10.42.N.0/24; the node
//	              itself is .1 in its /24.
//	10.43.0.0/16  Internal service VIPs (cluster-only).
//	10.44.0.0/16  Block-internal addresses (microvms/containers, Phase 09 —
//	              reserved, not used in this phase).
//
// External VIPs are allocated from a user-declared LAN pool, not from here.
const (
	// OverlayCIDR is the WireGuard overlay range.
	OverlayCIDR = "10.42.0.0/16"
	// InternalVIPCIDR is the internal service VIP range.
	InternalVIPCIDR = "10.43.0.0/16"
	// BlockInternalCIDR is reserved for Phase 09 workloads.
	BlockInternalCIDR = "10.44.0.0/16"

	// WireGuardPort is the fixed mesh listen port (§4.1).
	WireGuardPort = 51820
)

// OverlayPrefix returns node N's /24 in the overlay: node N gets
// 10.42.N.0/24. Valid for N in [1, 254]; node indices are 1-based to match
// the spec's diagram (n1 = 10.42.1.1) and keep the plan notation honest.
func OverlayPrefix(nodeIndex int) (netip.Prefix, error) {
	if nodeIndex < 1 || nodeIndex > 254 {
		return netip.Prefix{}, fmt.Errorf("node index %d out of range [1, 254] for overlay %s", nodeIndex, OverlayCIDR)
	}
	return netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 42, byte(nodeIndex), 0}), 24), nil
}

// OverlayAddress returns node N's own address: .1 in its /24.
func OverlayAddress(nodeIndex int) (netip.Addr, error) {
	p, err := OverlayPrefix(nodeIndex)
	if err != nil {
		return netip.Addr{}, err
	}
	return p.Addr().Next(), nil // .0 -> .1
}
