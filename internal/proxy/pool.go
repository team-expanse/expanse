// Package proxy implements the load balancer (PHASE05.md §4.3): the L4
// splice proxy and the L7 reverse proxy, both config-source-free — the
// routing table is derived from store watch events and rebuilt
// atomically (an immutable table behind an atomic.Pointer, swapped
// whole) so in-flight requests never observe a half-updated view.
//
// This file is the backend pool (T10): it watches /blocks/ and
// maintains the set of VIP-exposed services and their healthy backends.
// It performs no network I/O.
package proxy

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// BlockPrefix is the store subtree the pool watches. It contains block
// specs (binary pb.Block), observed status (binary pb.BlockStatus at
// <block>/status), and per-replica health records (JSON at
// <block>/status/replicas/<i>). Literal-prefix semantics: watch with
// the trailing slash.
const BlockPrefix = store.Key("/blocks/")

// primaryLeasePrefix is the store subtree of PostgreSQL primary-election
// lease records (internal/blocks/pgha.LeaseName's "pg-primary:<ref>"
// format under lease.Prefix). Local copy of the naming convention, not
// an import of internal/blocks/pgha or internal/cluster/lease — same
// decoupling healthRecord below already uses for internal/blocks/health.
const primaryLeasePrefix = store.Key("/leases/pg-primary:")

// statusSuffix marks the observed-state key of a block.
const statusSuffix = "/status"

// replicasDir marks the per-replica health record subtree.
const replicasDir = "/status/replicas/"

// Source is the store surface the pool needs. store.Store satisfies it.
type Source interface {
	Get(ctx context.Context, k store.Key) (*store.Entry, error)
	List(ctx context.Context, prefix store.Key) ([]*store.Entry, error)
	Watch(ctx context.Context, prefix store.Key, fromRev store.Revision) (<-chan store.Event, error)
	Revision(ctx context.Context) (store.Revision, error)
}

// Backend is one replica target in the immutable routing table.
type Backend struct {
	ReplicaIndex int32
	NodeID       string
	// Healthy is the readiness gate: a placement in phase RUNNING whose
	// health record (if one exists at all) reports ok. A MISSING record
	// counts as healthy — the record is only written when a readiness
	// probe is configured; placement bookkeeping is the primary gate.
	Healthy bool
}

// Service is one VIP-exposed port of one block.
type Service struct {
	Key        string // "<namespace>/<name>"
	Namespace  string
	Name       string
	Type       string // spec.type, e.g. "db/postgres" — lets LB wiring pick a routing mode per block kind
	Port       int32  // the port the VIP listens on
	TargetPort int32  // the replica port the VIP forwards to
	// Backends sorted by ReplicaIndex; never mutated after publish.
	Backends []Backend
	// HTTPRoutes are the L7 route declarations of the service's VIP
	// port (§4.3). Empty = L4 only (the default host route the L7
	// builder derives is never used for this service).
	HTTPRoutes []HTTPRouteDecl
	// PrimaryNodeID is the node the pg-primary election lease (D2, if
	// any) currently names, or "" if no election has decided yet. Only
	// meaningful for block types that run such an election.
	PrimaryNodeID string
}

// Primary returns the backend PrimaryNodeID names, if it is currently a
// healthy backend of this service. False when no election has decided,
// or the elected node's replica is not (or no longer) healthy — callers
// must not guess a fallback, the same "do not misroute a write" rule D2
// exists for.
func (s *Service) Primary() (Backend, bool) {
	if s.PrimaryNodeID == "" {
		return Backend{}, false
	}
	for _, b := range s.Healthy() {
		if b.NodeID == s.PrimaryNodeID {
			return b, true
		}
	}
	return Backend{}, false
}

// HTTPRouteDecl is one store-declared L7 route: Host + PathPrefix
// select requests; Service names the target block in the same
// namespace (empty = the declaring block itself).
type HTTPRouteDecl struct {
	Host       string
	PathPrefix string
	Service    string
}

// Table is an immutable snapshot of the routing state. Readers get the
// pointer via Pool.Table and must not mutate it; every rebuild produces
// a fresh Table.
type Table struct {
	Services map[string]*Service
}

// Service returns the service for a block key ("ns/name"), or nil.
func (t *Table) Service(key string) *Service {
	if t == nil {
		return nil
	}
	return t.Services[key]
}

// Healthy returns the healthy backends of a service, in index order.
func (s *Service) Healthy() []Backend {
	out := make([]Backend, 0, len(s.Backends))
	for _, b := range s.Backends {
		if b.Healthy {
			out = append(out, b)
		}
	}
	return out
}

// Pool watches /blocks/ and the pg-primary election leases, and
// rebuilds the routing table on every relevant event. Run drives the
// watch loops; Table is safe for concurrent readers at all times.
type Pool struct {
	src   Source
	table atomic.Pointer[Table]

	// Ingest state keyed by raw store key, guarded by mu: two watch
	// loops (blocks, primary leases) ingest and publish concurrently.
	mu      sync.Mutex
	blocks  map[string]*pb.Block
	status  map[string]*pb.BlockStatus
	health  map[string]*healthRecord // raw key → parsed record
	primary map[string]string        // block ref ("ns/name") → pg-primary lease holder
}

// healthRecord mirrors internal/blocks/health.Record. Local copy (not
// an import) keeps the proxy package decoupled from the agent's probe
// implementation; the JSON shape is the contract.
type healthRecord struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// leaseRecord mirrors internal/cluster/lease's private wire format for
// /leases/<name> keys. Local copy, not an import of
// internal/cluster/lease: the pool only needs the holder, not the
// exported lease.Manager API (same rationale as healthRecord above).
type leaseRecord struct {
	Holder string `json:"h"`
}

// NewPool returns a pool over src. Run must be called before Table
// reflects store contents.
func NewPool(src Source) *Pool {
	return &Pool{
		src:     src,
		blocks:  map[string]*pb.Block{},
		status:  map[string]*pb.BlockStatus{},
		health:  map[string]*healthRecord{},
		primary: map[string]string{},
	}
}

// Run seeds from a List snapshot, then applies watch events until ctx
// is done or either watch channel closes (overflow/close semantics: the
// caller re-lists, matching internal/store's re-list contract).
func (p *Pool) Run(ctx context.Context) error {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 2)
	go func() { errCh <- p.watch(cctx, BlockPrefix, p.ingestBlock, p.removeBlock) }()
	go func() { errCh <- p.watch(cctx, primaryLeasePrefix, p.ingestPrimary, p.removePrimary) }()
	err := <-errCh
	cancel() // one loop ending must stop the other too, not leak it
	<-errCh
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// watch runs one prefix's seed-then-apply loop, calling ingest/remove
// per entry and publish after each batch. Shared by the block watch and
// the primary-lease watch; both feed the same Table via publish's lock.
func (p *Pool) watch(ctx context.Context, prefix store.Key, ingest func(string, []byte), remove func(string)) error {
	cur, err := p.src.Revision(ctx)
	if err != nil {
		return err
	}
	ch, err := p.src.Watch(ctx, prefix, cur)
	if err != nil {
		return err
	}
	// Seed: everything at or below cur is already covered by the watch
	// window (events AFTER cur only).
	ents, err := p.src.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, e := range ents {
		ingest(string(e.Key), e.Value)
	}
	p.publish()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return store.ErrNotFound // watch closed; caller restarts Run
			}
			if ev.Entry != nil {
				if ev.Type == store.EventDelete {
					remove(string(ev.Entry.Key))
				} else {
					ingest(string(ev.Entry.Key), ev.Entry.Value)
				}
				p.publish()
			}
		}
	}
}

// ingestBlock parses one /blocks/ entry into the right bucket by key
// shape.
func (p *Pool) ingestBlock(key string, val []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case strings.HasSuffix(key, statusSuffix):
		var st pb.BlockStatus
		if proto.Unmarshal(val, &st) == nil {
			p.status[key] = &st
		}
	case strings.Contains(key, replicasDir):
		var rec healthRecord
		if json.Unmarshal(val, &rec) == nil {
			p.health[key] = &rec
		}
	default:
		var b pb.Block
		if proto.Unmarshal(val, &b) == nil {
			p.blocks[key] = &b
		}
	}
}

// removeBlock drops a /blocks/ key from every bucket it might occupy.
func (p *Pool) removeBlock(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.blocks, key)
	delete(p.status, key)
	for k := range p.health {
		if strings.HasPrefix(k, key+"/") {
			delete(p.health, k)
		}
	}
}

// ingestPrimary records a pg-primary lease's current holder, keyed by
// the block ref the lease name encodes.
func (p *Pool) ingestPrimary(key string, val []byte) {
	ref := strings.TrimPrefix(key, string(primaryLeasePrefix))
	var rec leaseRecord
	if json.Unmarshal(val, &rec) != nil || rec.Holder == "" {
		return
	}
	p.mu.Lock()
	p.primary[ref] = rec.Holder
	p.mu.Unlock()
}

// removePrimary drops a pg-primary lease record (released or expired
// and deleted by its owner).
func (p *Pool) removePrimary(key string) {
	ref := strings.TrimPrefix(key, string(primaryLeasePrefix))
	p.mu.Lock()
	delete(p.primary, ref)
	p.mu.Unlock()
}

// publish rebuilds the whole table from ingest state and swaps it in
// atomically. The old table stays valid for any reader that already
// holds its pointer.
func (p *Pool) publish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := &Table{Services: map[string]*Service{}}
	for key, b := range p.blocks {
		ref := strings.TrimPrefix(key, "/blocks/")
		slash := strings.Index(ref, "/")
		if slash < 0 {
			continue
		}
		port := firstVIPPort(b)
		if port == nil {
			continue
		}
		target := port.GetTargetPort()
		if target == 0 {
			target = port.GetPort()
		}
		st := p.status[key+"/status"]
		svc := &Service{
			Key:           ref,
			Namespace:     ref[:slash],
			Name:          ref[slash+1:],
			Type:          b.GetSpec().GetType(),
			Port:          port.GetPort(),
			TargetPort:    target,
			PrimaryNodeID: p.primary[ref],
		}
		for _, decl := range port.GetHttpRoutes() {
			svc.HTTPRoutes = append(svc.HTTPRoutes, HTTPRouteDecl{
				Host:       decl.GetHost(),
				PathPrefix: decl.GetPathPrefix(),
				Service:    decl.GetService(),
			})
		}
		for _, pl := range st.GetPlacements() {
			if pl.GetPhase() != pb.Phase_RUNNING {
				continue
			}
			svc.Backends = append(svc.Backends, Backend{
				ReplicaIndex: pl.GetReplicaIndex(),
				NodeID:       pl.GetNodeId(),
				Healthy:      p.replicaHealthy(key, pl.GetReplicaIndex()),
			})
		}
		sort.Slice(svc.Backends, func(i, j int) bool {
			return svc.Backends[i].ReplicaIndex < svc.Backends[j].ReplicaIndex
		})
		t.Services[ref] = svc
	}
	p.table.Store(t)
}

// replicaHealthy reports the readiness of one replica: unhealthy only
// when a health record exists and says so (missing record = healthy —
// see Backend.Healthy).
func (p *Pool) replicaHealthy(blockKey string, idx int32) bool {
	rec, ok := p.health[healthKey(blockKey, idx)]
	if !ok {
		return true
	}
	return rec.OK
}

func healthKey(blockKey string, idx int32) string {
	return blockKey + replicasDir + itoa(int(idx))
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

// Table returns the current immutable snapshot. Never nil after the
// first publish; before Run's seed completes it is an empty table.
func (p *Pool) Table() *Table {
	if t := p.table.Load(); t != nil {
		return t
	}
	return &Table{Services: map[string]*Service{}}
}

// firstVIPPort picks the block's VIP-exposed port (same policy as the
// agent's VIP scan: the first EXPOSE_VIP port in spec order).
func firstVIPPort(b *pb.Block) *pb.Port {
	for _, p := range b.GetSpec().GetNetwork().GetPorts() {
		if p.GetExpose() == pb.Expose_EXPOSE_VIP {
			return p
		}
	}
	return nil
}
