// source.go drives the zone from the store (T17): it watches the
// store subtree, re-runs the T16 Build on every event, and swaps the
// result into the Server atomically — the same rebuild-and-swap
// discipline the proxy pool uses, targeting the spec's ≤5 s
// propagation bound (TTL 5 s plus one watch delivery).
package dns

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"

	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// StoreSource is the store surface the zone builder needs; store.Store
// satisfies it.
type StoreSource interface {
	List(ctx context.Context, prefix store.Key) ([]*store.Entry, error)
	Watch(ctx context.Context, prefix store.Key, fromRev store.Revision) (<-chan store.Event, error)
	Revision(ctx context.Context) (store.Revision, error)
}

// ZoneSource watches the store and keeps the Server's zone current.
type ZoneSource struct {
	src    StoreSource
	server *Server

	// Ingest state keyed by raw store key; mutated only by the single
	// watch goroutine, read only in rebuild.
	blocks  map[string]*pb.Block
	status  map[string]*pb.BlockStatus
	health  map[string]*healthRecord
	indexes map[int]string          // overlay index → nodeID
	vips    map[string]netip.Prefix // /network/vipPool/<scope>/<ref> → addr

	// lastInput is the rebuilt snapshot, kept for the mDNS bridge's
	// plan derivation (it needs the MDNS opt-in flags, which the flat
	// record list does not carry).
	lastInput Input
}

// LastInput returns the most recently rebuilt zone input.
func (z *ZoneSource) LastInput() Input {
	return z.lastInput
}

// healthRecord mirrors the health record JSON contract.
type healthRecord struct {
	OK bool `json:"ok"`
}

// NewZoneSource wires a zone source to a server.
func NewZoneSource(src StoreSource, server *Server) *ZoneSource {
	return &ZoneSource{
		src:     src,
		server:  server,
		blocks:  map[string]*pb.Block{},
		status:  map[string]*pb.BlockStatus{},
		health:  map[string]*healthRecord{},
		indexes: map[int]string{},
		vips:    map[string]netip.Prefix{},
	}
}

// Run seeds from a List snapshot and then applies watch events until
// ctx is done. The watch is on "/" — the zone depends on blocks,
// statuses, health, node indexes, and VIP allocations, and re-listing
// on overflow is simpler than juggling five watches.
func (z *ZoneSource) Run(ctx context.Context) error {
	cur, err := z.src.Revision(ctx)
	if err != nil {
		return err
	}
	ch, err := z.src.Watch(ctx, "/", cur)
	if err != nil {
		return err
	}
	ents, err := z.src.List(ctx, "/")
	if err != nil {
		return err
	}
	for _, e := range ents {
		z.ingest(string(e.Key), e.Value)
	}
	z.rebuild()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return store.ErrNotFound // watch closed; caller restarts
			}
			if ev.Entry != nil {
				if ev.Type == store.EventDelete {
					z.remove(string(ev.Entry.Key))
				} else {
					z.ingest(string(ev.Entry.Key), ev.Entry.Value)
				}
				z.rebuild()
			}
		}
	}
}

func (z *ZoneSource) ingest(key string, val []byte) {
	switch {
	case strings.HasPrefix(key, "/blocks/"):
		switch {
		case strings.HasSuffix(key, "/status"):
			var st pb.BlockStatus
			if proto.Unmarshal(val, &st) == nil {
				z.status[key] = &st
			}
		case strings.Contains(key, "/status/replicas/"):
			var rec healthRecord
			if json.Unmarshal(val, &rec) == nil {
				z.health[key] = &rec
			}
		default:
			var b pb.Block
			if proto.Unmarshal(val, &b) == nil {
				z.blocks[key] = &b
			}
		}
	case strings.HasPrefix(key, "/network/nodeIndexes/"):
		var idx int
		if _, err := fmt.Sscanf(key, "/network/nodeIndexes/%d", &idx); err != nil {
			return
		}
		z.indexes[idx] = string(val)
	case strings.HasPrefix(key, "/network/vipPool/"):
		var a struct {
			Addr string `json:"addr"`
		}
		if json.Unmarshal(val, &a) == nil && a.Addr != "" {
			if p, err := netip.ParsePrefix(a.Addr); err == nil {
				z.vips[strings.TrimPrefix(key, "/network/vipPool/")] = p
			}
		}
	}
}

func (z *ZoneSource) remove(key string) {
	switch {
	case strings.HasPrefix(key, "/blocks/"):
		delete(z.blocks, key)
		delete(z.status, key)
		for k := range z.health {
			if strings.HasPrefix(k, key+"/") {
				delete(z.health, k)
			}
		}
	case strings.HasPrefix(key, "/network/nodeIndexes/"):
		var idx int
		if _, err := fmt.Sscanf(key, "/network/nodeIndexes/%d", &idx); err == nil {
			delete(z.indexes, idx)
		}
	case strings.HasPrefix(key, "/network/vipPool/"):
		delete(z.vips, strings.TrimPrefix(key, "/network/vipPool/"))
	}
}

// rebuild maps the ingest state onto the T16 builder's Input and swaps
// the result into the server. Node overlay addresses follow the
// address plan: index N → 10.42.N.1 (the address EnsureAddr puts on
// exp0).
func (z *ZoneSource) rebuild() {
	in := Input{}

	nodes := map[string]*NodeInfo{} // nodeID → info
	for idx, id := range z.indexes {
		n := &NodeInfo{ID: id, Addrs: []netip.Addr{
			netip.AddrFrom4([4]byte{10, 42, byte(idx), 1}),
		}}
		nodes[id] = n
	}

	for key, b := range z.blocks {
		ref := strings.TrimPrefix(key, "/blocks/")
		slash := strings.Index(ref, "/")
		if slash < 0 {
			continue
		}
		bi := BlockInfo{
			Namespace: ref[:slash],
			Name:      ref[slash+1:],
			MDNS:      b.GetSpec().GetNetwork().GetMdns(),
		}
		if vip, ok := z.vips["internal/"+ref]; ok {
			bi.VIP = vip
		}
		for _, p := range b.GetSpec().GetNetwork().GetPorts() {
			bi.Ports = append(bi.Ports, PortInfo{
				Name: p.GetName(), Port: p.GetPort(), TargetPort: p.GetTargetPort(),
			})
		}
		for _, pl := range z.status[key+"/status"].GetPlacements() {
			if pl.GetPhase() != pb.Phase_RUNNING {
				continue
			}
			r := Replica{Index: pl.GetReplicaIndex(), Node: nodes[pl.GetNodeId()], Ready: true}
			if rec, ok := z.health[key+"/status/replicas/"+fmt.Sprint(pl.GetReplicaIndex())]; ok && !rec.OK {
				r.Ready = false
			}
			bi.Replicas = append(bi.Replicas, r)
		}
		in.Blocks = append(in.Blocks, bi)
	}
	for _, n := range nodes {
		in.Nodes = append(in.Nodes, *n)
	}
	z.lastInput = in
	z.server.SetZone(Build(in))
}
