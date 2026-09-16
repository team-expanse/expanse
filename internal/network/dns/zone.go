// Package dns implements cluster DNS (PHASE05.md §4.4): an
// authoritative server for expanse.local. built from store state.
//
// This file is the zone builder (T16): a pure function from the
// current block/node store state to the record set. It performs no
// network I/O and holds no locks — the T17 server re-runs it on every
// store watch event and serves the resulting immutable zone atomically
// (the same rebuild-and-swap discipline the proxy pool uses).
//
// Record table (spec §4.4):
//
//	<block>.<ns>.expanse.local.        A    the block's VIP, or every
//	                                        ready replica's node overlay
//	                                        IP if the block has no VIP
//	_<name>._tcp.<block>.<ns>.expanse.local.
//	                                   SRV  per named port, one record
//	                                        per ready replica
//	<i>.<block>.<ns>.expanse.local.    A    individual ready replica
//	                                        (stateful direct access)
//	<node>.nodes.expanse.local.        A    node overlay IP
//	cluster.expanse.local.             A    all node overlay IPs
//	                                        (round-robin entry point)
//
// TTLs: 5 s for block records (fast failover), 300 s for node records.
// A block with zero ready replicas contributes no records at all — a
// missing record is the correct negative answer, not an error
// (daemonsets and singletons hit this during rescheduling).
package dns

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// Zone is the authoritative zone the builder serves.
const Zone = "expanse.local."

// TTLs per the spec: block records fail over fast; node topology is
// effectively static between joins and gets a long TTL.
const (
	BlockTTL = 5
	NodeTTL  = 300
)

// NodeInfo is one node's stable identity as the builder needs it: its
// ID (DNS label) and overlay address (10.42.<index>.1 per the address
// plan — the same address the mesh assigns to exp0).
type NodeInfo struct {
	ID    string
	Addrs []netip.Addr // overlay address first; extra addresses ignored today
}

// Replica is one placement of a block. Ready is the caller's readiness
// verdict (phase RUNNING + health record ok-or-missing, mirroring the
// proxy pool's Backend.Healthy gate).
type Replica struct {
	Index int32
	Node  *NodeInfo
	Ready bool
}

// PortInfo is one declared port of a block. Only named ports produce
// SRV records; the port clients dial on the replica is TargetPort
// (falling back to Port when unset, matching the proxy pool's rule).
type PortInfo struct {
	Name       string
	Port       int32
	TargetPort int32
}

// BlockInfo is one block's routing-relevant state.
type BlockInfo struct {
	Namespace string
	Name      string
	// VIP is the block's allocated VIP, if any (zero Addr = none).
	VIP netip.Prefix
	// Ports is the declared port list (spec order preserved).
	Ports []PortInfo
	// MDNS mirrors the block's network.mdns opt-in for the bridge.
	MDNS bool
	// Replicas is the placement list (any order; output is sorted).
	Replicas []Replica
}

// Input is the full store snapshot the zone is derived from.
type Input struct {
	Blocks []BlockInfo
	Nodes  []NodeInfo
}

// Record is one flattened DNS record. Data is the presentation-format
// RDATA: an address for A, "prio weight port target" for SRV.
type Record struct {
	Name string // FQDN with trailing dot
	Type string // "A" or "SRV"
	TTL  uint32
	Data string
}

// ReplicaPort is the replica port a PortInfo dials, matching the
// proxy pool's defaulting rule (target_port 0 = same as the service
// port).
func (p PortInfo) ReplicaPort() int32 {
	if p.TargetPort == 0 {
		return p.Port
	}
	return p.TargetPort
}

// fqdn joins labels under the zone.
func fqdn(labels ...string) string {
	return strings.Join(labels, ".") + "." + Zone
}

// Build derives the complete record set. The output is deterministic:
// sorted by (name, type, data), so callers can diff zones and the
// server can serve consistent answers regardless of map iteration
// order. Malformed inputs (empty labels) are skipped, not errors —
// the store is the source of truth and garbage keys must not take the
// whole zone down.
func Build(in Input) []Record {
	var out []Record

	for _, n := range in.Nodes {
		if n.ID == "" || len(n.Addrs) == 0 {
			continue
		}
		out = append(out, Record{
			Name: fqdn(n.ID, "nodes"),
			Type: "A",
			TTL:  NodeTTL,
			Data: n.Addrs[0].String(),
		})
	}
	// cluster.expanse.local: every node, sorted by ID for stable
	// round-robin ordering.
	nodes := make([]NodeInfo, len(in.Nodes))
	copy(nodes, in.Nodes)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	for _, n := range nodes {
		if n.ID == "" || len(n.Addrs) == 0 {
			continue
		}
		out = append(out, Record{
			Name: fqdn("cluster"),
			Type: "A",
			TTL:  NodeTTL,
			Data: n.Addrs[0].String(),
		})
	}

	for _, b := range in.Blocks {
		if b.Namespace == "" || b.Name == "" {
			continue
		}
		base := fqdn(b.Name, b.Namespace)
		blockTTL := uint32(BlockTTL)

		// Ready replicas in index order — the stable order every
		// derived list below follows.
		type readyReplica struct {
			idx  int32
			addr netip.Addr
		}
		var ready []readyReplica
		for _, r := range b.Replicas {
			if !r.Ready || r.Node == nil || len(r.Node.Addrs) == 0 {
				continue
			}
			ready = append(ready, readyReplica{idx: r.Index, addr: r.Node.Addrs[0]})
		}
		sort.Slice(ready, func(i, j int) bool { return ready[i].idx < ready[j].idx })
		if len(ready) == 0 {
			continue // no A record — the correct negative answer
		}

		// Block A record: VIP if allocated, else every ready replica's
		// node overlay IP (deduplicated — two replicas on one node are
		// one address).
		if b.VIP.IsValid() {
			out = append(out, Record{Name: base, Type: "A", TTL: blockTTL, Data: b.VIP.Addr().String()})
		} else {
			seen := map[netip.Addr]bool{}
			for _, r := range ready {
				if seen[r.addr] {
					continue
				}
				seen[r.addr] = true
				out = append(out, Record{Name: base, Type: "A", TTL: blockTTL, Data: r.addr.String()})
			}
		}

		// Per-replica A records for stateful direct access.
		for _, r := range ready {
			out = append(out, Record{
				Name: fqdn(fmt.Sprint(r.idx), b.Name, b.Namespace),
				Type: "A",
				TTL:  blockTTL,
				Data: r.addr.String(),
			})
		}

		// SRV per named port, one record per ready replica.
		for _, p := range b.Ports {
			if p.Name == "" {
				continue
			}
			srvName := fqdn("_"+p.Name+"._tcp", b.Name, b.Namespace)
			for _, r := range ready {
				out = append(out, Record{
					Name: srvName,
					Type: "SRV",
					TTL:  blockTTL,
					Data: fmt.Sprintf("0 0 %d %s", p.ReplicaPort(),
						fqdn(fmt.Sprint(r.idx), b.Name, b.Namespace)),
				})
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Data < out[j].Data
	})
	return out
}

// VIPFromAllocation parses a /network/vipPool allocation record into
// the prefix the builder expects. It exists here (rather than making
// callers depend on vip internals) so the agent's T17 watch loop can
// feed Build straight from store values.
func VIPFromAllocation(val []byte) (netip.Prefix, bool) {
	var a struct {
		Addr string `json:"addr"`
	}
	if err := json.Unmarshal(val, &a); err != nil || a.Addr == "" {
		return netip.Prefix{}, false
	}
	p, err := netip.ParsePrefix(a.Addr)
	if err != nil {
		return netip.Prefix{}, false
	}
	return p, true
}
