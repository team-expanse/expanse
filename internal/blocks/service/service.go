// Package service implements the BlockService CRUD API (PHASE04.md §7):
// Create/Get/List/Update/Delete over a replicated raftstore.Store.
//
// Store schema: key /blocks/<namespace>/<name>, value = the proto-binary
// encoding of Block (proto.Marshal). Proto-binary, not JSON: it round-trips
// the exact proto types losslessly (including google.protobuf.Struct and
// enum presence), is schema-versioned by the proto evolution rules, and is
// what the FSM already hashes for generation snapshots. Human readability
// is served by the API layer (Get/List), never the store.
//
// Contract honored from Phase03 §4.7 / T13: every accepted Create/Update/
// Delete mutates a key under /blocks/ — a DesiredPrefixes member — inside a
// single Raft transaction, so the FSM's generation snapshotter bumps the
// generation in the SAME transaction as the block write. A rejected spec
// performs zero store writes and therefore zero generation bumps.
//
// V2 (name unique within namespace) is enforced with an OpCheck(Expect=0)
// inside the Create transaction — the authoritative check, immune to races;
// the pre-txn admission pass (V1–V24) exists to give good error messages.
package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/expanse/expanse/internal/blocks/validate"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/scheduler"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// BlockPrefix is the replicated-store prefix for all blocks.
const BlockPrefix = "/blocks/"

// Server implements pb.BlockServiceServer for the CRUD subset.
// Scale/Restart/Watch/StreamLogs/Explain arrive in later cards.
type Server struct {
	pb.UnimplementedBlockServiceServer

	// St is the replicated store (raft-backed; writes are Raft log entries).
	St store.Store
	// Admission builds the validate.Context per request. Returning a
	// context with nil Catalog skips V3/V19 (test-only escape hatch);
	// production wires the real catalog.
	Admission func() validate.Context
	// Nodes supplies the current cluster view for Explain (T18).
	// Production wiring arrives with the Phase 05 node adapter.
	Nodes func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error)
}

// New builds a Server.
func New(st store.Store, admission func() validate.Context) *Server {
	return &Server{St: st, Admission: admission}
}

// key is the store key for a block.
func key(namespace, name string) store.Key {
	return store.Key(BlockPrefix + namespace + "/" + name)
}

func (s *Server) adCtx() validate.Context {
	if s.Admission == nil {
		return validate.Context{}
	}
	return s.Admission()
}

// admissionErr converts the first validation failure to a typed error.
func admissionErr(ves []validate.ValidationError) error {
	if len(ves) == 0 {
		return nil
	}
	v := ves[0]
	return errors.New(errors.KindInvalid, "blocks.service", v.Error())
}

// Create admits and persists a new block. The block is written and the
// generation bumped in one Raft transaction; BlockStatus is seeded
// Phase=PENDING with ObservedGeneration set to the generation this create
// produces (current + 1; the FSM bumps exactly once for this txn).
func (s *Server) Create(ctx context.Context, b *pb.Block) (*pb.Block, error) {
	if b.GetMetadata().GetName() == "" {
		return nil, errors.New(errors.KindInvalid, "blocks.service.Create", "name is required")
	}
	if b.GetMetadata().GetNamespace() == "" {
		b = proto.Clone(b).(*pb.Block)
		b.Metadata.Namespace = "default"
	}
	k := key(b.GetMetadata().GetNamespace(), b.GetMetadata().GetName())

	// Pre-txn admission for good error messages (V1–V24, incl. a
	// advisory V2 via Existing). Zero store writes happen on failure.
	ad := s.adCtx()
	ad.Existing = s.namesInNamespace(ctx, b.GetMetadata().GetNamespace(), "")
	if ves := validate.Validate(b, ad); len(ves) > 0 {
		return nil, admissionErr(ves)
	}

	// Seed system status. ObservedGeneration: the FSM snapshots the new
	// generation from this same txn, so the block is born observed.
	gen, err := generationCurrent(ctx, s.St)
	if err != nil {
		return nil, err
	}
	b.Status = &pb.BlockStatus{Phase: pb.Phase_PENDING, ObservedGeneration: int64(gen + 1)}

	out, err := proto.Marshal(b)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "blocks.service.Create", "marshal block")
	}
	if _, err := s.St.Txn(ctx, []store.Op{
		{Kind: store.OpCheck, Key: k, Expect: 0}, // V2, atomic read-before-write
		{Kind: store.OpPut, Key: k, Value: out},
	}); err != nil {
		return nil, errors.Wrap(err, errors.KindConflict, "blocks.service.Create",
			fmt.Sprintf("block %s/%s already exists or txn failed", b.GetMetadata().GetNamespace(), b.GetMetadata().GetName()))
	}
	return b, nil
}

// Get fetches one block.
func (s *Server) Get(ctx context.Context, r *pb.GetBlockRequest) (*pb.Block, error) {
	if r.GetName() == "" {
		return nil, errors.New(errors.KindInvalid, "blocks.service.Get", "name is required")
	}
	return s.get(ctx, r.GetNamespace(), r.GetName())
}

func (s *Server) get(ctx context.Context, ns, name string) (*pb.Block, error) {
	if ns == "" {
		ns = "default"
	}
	e, err := s.St.Get(ctx, key(ns, name))
	if err != nil {
		return nil, errors.Wrap(err, errors.KindNotFound, "blocks.service.Get",
			fmt.Sprintf("block %s/%s", ns, name))
	}
	var b pb.Block
	if err := proto.Unmarshal(e.Value, &b); err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "blocks.service.Get", "unmarshal block")
	}
	return &b, nil
}

// List returns blocks, optionally filtered by namespace (empty = all).
func (s *Server) List(ctx context.Context, r *pb.ListBlocksRequest) (*pb.ListBlocksResponse, error) {
	prefix := BlockPrefix
	if r.GetNamespace() != "" {
		prefix += r.GetNamespace() + "/"
	}
	entries, err := s.St.List(ctx, store.Key(prefix))
	if err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "blocks.service.List", "list blocks")
	}
	resp := &pb.ListBlocksResponse{}
	for _, e := range entries {
		var b pb.Block
		if err := proto.Unmarshal(e.Value, &b); err != nil {
			return nil, errors.Wrap(err, errors.KindInternal, "blocks.service.List", "unmarshal "+string(e.Key))
		}
		resp.Blocks = append(resp.Blocks, &b)
	}
	sort.Slice(resp.Blocks, func(i, j int) bool {
		a, c := resp.Blocks[i].GetMetadata(), resp.Blocks[j].GetMetadata()
		return a.GetNamespace()+"--"+a.GetName() < c.GetNamespace()+"--"+c.GetName()
	})
	return resp, nil
}

// Update replaces the spec of an existing block (full replace). System
// status is preserved — users never write status. Same single-Raft-txn
// generation contract as Create.
func (s *Server) Update(ctx context.Context, b *pb.Block) (*pb.Block, error) {
	if b.GetMetadata().GetName() == "" {
		return nil, errors.New(errors.KindInvalid, "blocks.service.Update", "name is required")
	}
	if b.GetMetadata().GetNamespace() == "" {
		b = proto.Clone(b).(*pb.Block)
		b.Metadata.Namespace = "default"
	}
	ns, name := b.GetMetadata().GetNamespace(), b.GetMetadata().GetName()
	k := key(ns, name)

	// Read current for CAS revision and status preservation.
	cur, err := s.get(ctx, ns, name)
	if err != nil {
		return nil, err
	}
	e, err := s.St.Get(ctx, k)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "blocks.service.Update", "read for revision")
	}

	ad := s.adCtx()
	ad.Existing = s.namesInNamespace(ctx, ns, name)
	if ves := validate.Validate(b, ad); len(ves) > 0 {
		return nil, admissionErr(ves)
	}

	gen, err := generationCurrent(ctx, s.St)
	if err != nil {
		return nil, err
	}
	b.Status = cur.GetStatus()
	b.Status.ObservedGeneration = int64(gen + 1)

	out, err := proto.Marshal(b)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "blocks.service.Update", "marshal block")
	}
	if _, err := s.St.Txn(ctx, []store.Op{
		{Kind: store.OpCheck, Key: k, Expect: e.Revision}, // must still exist unmodified
		{Kind: store.OpPut, Key: k, Value: out},
	}); err != nil {
		return nil, errors.Wrap(err, errors.KindConflict, "blocks.service.Update",
			fmt.Sprintf("block %s/%s modified concurrently", ns, name))
	}
	return b, nil
}

// Delete removes a block. Same single-Raft-txn generation contract.
func (s *Server) Delete(ctx context.Context, r *pb.DeleteBlockRequest) (*emptypb.Empty, error) {
	if r.GetName() == "" {
		return nil, errors.New(errors.KindInvalid, "blocks.service.Delete", "name is required")
	}
	ns := r.GetNamespace()
	if ns == "" {
		ns = "default"
	}
	k := key(ns, r.GetName())
	e, err := s.St.Get(ctx, k)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindNotFound, "blocks.service.Delete",
			fmt.Sprintf("block %s/%s", ns, r.GetName()))
	}
	var b pb.Block
	if err := proto.Unmarshal(e.Value, &b); err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "blocks.service.Delete", "unmarshal block")
	}
	if _, err := s.St.Txn(ctx, []store.Op{
		{Kind: store.OpCheck, Key: k, Expect: e.Revision},
		{Kind: store.OpDelete, Key: k},
	}); err != nil {
		return nil, errors.Wrap(err, errors.KindConflict, "blocks.service.Delete",
			fmt.Sprintf("block %s/%s modified concurrently", ns, r.GetName()))
	}
	return &emptypb.Empty{}, nil
}

// namesInNamespace lists existing block names in ns, excluding the named
// block (for V2's Context.Existing).
func (s *Server) namesInNamespace(ctx context.Context, ns, exclude string) []string {
	entries, err := s.St.List(ctx, store.Key(BlockPrefix+ns+"/"))
	if err != nil {
		return nil // V2's authoritative check is the txn OpCheck anyway
	}
	var names []string
	for _, e := range entries {
		p := strings.TrimPrefix(string(e.Key), BlockPrefix+ns+"/")
		if p != exclude && p != "" {
			names = append(names, p)
		}
	}
	return names
}

// generationCurrent reads the current generation number (0 if none).
func generationCurrent(ctx context.Context, st store.Store) (uint64, error) {
	e, err := st.Get(ctx, "/cluster/generation")
	if err != nil {
		if errors.Is(err, errors.KindNotFound) || err == store.ErrNotFound {
			return 0, nil
		}
		return 0, errors.Wrap(err, errors.KindInternal, "blocks.service", "read generation")
	}
	var n uint64
	if _, err := fmt.Sscan(string(e.Value), &n); err != nil {
		return 0, errors.Wrap(err, errors.KindInternal, "blocks.service", "parse generation")
	}
	return n, nil
}
