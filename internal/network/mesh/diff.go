package mesh

import (
	"net/netip"
	"slices"
	"time"
)

// PeerSpec is the desired state for one WireGuard peer on exp0.
type PeerSpec struct {
	// NodeID is the peer's cluster node ID (informational).
	NodeID string
	// PublicKey is the peer's base64 WireGuard public key.
	PublicKey string
	// Endpoint is the peer's "host:port", possibly empty when unknown.
	Endpoint string
	// AllowedIPs is the peer's overlay /24 plus any VIPs it holds.
	AllowedIPs []netip.Prefix
	// PersistentKeepalive interval (§4.1: 25s). Not diffed — constant.
	Keepalive time.Duration
}

// PeerState is the observed state of one peer on the device, keyed by
// public key.
type PeerState struct {
	PublicKey  string
	Endpoint   string // "host:port" or "" when unset
	AllowedIPs []netip.Prefix
}

// OpKind classifies a convergence operation.
type OpKind string

const (
	OpAdd    OpKind = "add"
	OpUpdate OpKind = "update"
	OpRemove OpKind = "remove"
)

// Op is one incremental peer operation. Applying it must never disturb
// other peers (no full setconf replace — §4.1 reconciler integration).
type Op struct {
	Kind OpKind
	// Spec is set for add/update.
	Spec *PeerSpec
	// PublicKey is set for remove (the peer to delete from the device).
	PublicKey string
	// Reason is human-readable, for logs and doctor output.
	Reason string
}

// KeepaliveInterval is the §4.1 PersistentKeepalive for every peer.
const KeepaliveInterval = 25 * time.Second

// Diff produces the exact incremental operations that take the device
// from the observed peer set to the desired specs:
//
//   - a public key present on the device but absent from specs → remove
//   - a spec absent from the device → add
//   - a spec present but with a different endpoint or AllowedIPs set →
//     update (set-based compare: order-insensitive)
//   - unchanged peers produce no operation
//
// Determinism: ops are ordered remove, update, add — removals first so a
// peer being replaced (same AllowedIPs, new key) releases the route
// before its replacement claims it — then by public key for stable
// replay in tests and logs.
func Diff(state []PeerState, specs []PeerSpec) []Op {
	byKey := make(map[string]PeerSpec, len(specs))
	for _, s := range specs {
		byKey[s.PublicKey] = s
	}

	var remove, update, add []Op
	for _, p := range state {
		want, ok := byKey[p.PublicKey]
		if !ok {
			remove = append(remove, Op{
				Kind: OpRemove, PublicKey: p.PublicKey,
				Reason: "peer no longer in /nodes/",
			})
			continue
		}
		delete(byKey, p.PublicKey) // mark seen
		if !sameEndpoint(p.Endpoint, want.Endpoint) ||
			!PrefixesEqual(p.AllowedIPs, want.AllowedIPs) {
			w := want
			update = append(update, Op{
				Kind: OpUpdate, Spec: &w,
				Reason: "endpoint or AllowedIPs changed",
			})
		}
	}
	for _, s := range byKey { // remaining = not on device
		s := s
		add = append(add, Op{
			Kind: OpAdd, Spec: &s,
			Reason: "new mesh peer",
		})
	}

	slices.SortFunc(remove, opLess)
	slices.SortFunc(update, func(a, b Op) int { return cmpSpec(a.Spec, b.Spec) })
	slices.SortFunc(add, func(a, b Op) int { return cmpSpec(a.Spec, b.Spec) })
	return append(append(remove, update...), add...)
}

func opLess(a, b Op) int { return cmpStr(a.PublicKey, b.PublicKey) }

func cmpSpec(a, b *PeerSpec) int {
	if a == nil || b == nil {
		return 0
	}
	return cmpStr(a.PublicKey, b.PublicKey)
}

func cmpStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func sameEndpoint(got, want string) bool { return got == want }

// PrefixesEqual compares prefix sets ignoring order.
func PrefixesEqual(a, b []netip.Prefix) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]netip.Prefix(nil), a...)
	bc := append([]netip.Prefix(nil), b...)
	slices.SortFunc(ac, cmpPrefix)
	slices.SortFunc(bc, cmpPrefix)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

func cmpPrefix(a, b netip.Prefix) int {
	as, bs := a.String(), b.String()
	switch {
	case as < bs:
		return -1
	case as > bs:
		return 1
	}
	return 0
}
