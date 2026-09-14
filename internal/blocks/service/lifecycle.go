// Lifecycle-RPC surface of BlockService (T14): Scale, Restart, Watch —
// plus CatalogService (ListTypes/GetType) over T04's real catalog.
//
// Design notes:
//   - Scale and Restart follow the same write discipline as Create/Update:
//     re-validate at admission, preserve user-invisible status, CAS via
//     OpCheck on the read revision, all inside one Raft txn (so the FSM's
//     generation snapshotter bumps in the same transaction).
//   - Restart is modeled on `kubectl rollout restart`: it stamps the
//     metadata annotation "expanse.io/restartedAt" with the current
//     RFC3339 time. The reconcile loop (T10) picks the change up; the
//     per-replica restart machinery itself is Phase 05 (T20). This is the
//     documented emission point: no change to T09's pure Transition.
//   - Watch maps store events to BlockEvents: Put with Prev==nil →
//     EVENT_ADDED, Put with Prev → EVENT_MODIFIED, Delete → EVENT_DELETED.
//     Filtering by name is server-side; empty name watches the namespace.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/expanse/expanse/internal/blocks/catalog"
	"github.com/expanse/expanse/internal/blocks/validate"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// RestartedAtAnnotation stamps a restart request (see package notes).
const RestartedAtAnnotation = "expanse.io/restartedAt"

// Scale changes a block's replica count. Same txn contract as Update.
func (s *Server) Scale(ctx context.Context, r *pb.ScaleRequest) (*pb.Block, error) {
	if r.GetName() == "" {
		return nil, errors.New(errors.KindInvalid, "blocks.service.Scale", "name is required")
	}
	if r.GetReplicas() < 0 {
		return nil, errors.New(errors.KindInvalid, "blocks.service.Scale", "replicas must be >= 0 (V4)")
	}
	ns := r.GetNamespace()
	if ns == "" {
		ns = "default"
	}
	b, err := s.get(ctx, ns, r.GetName())
	if err != nil {
		return nil, err
	}
	e, err := s.St.Get(ctx, key(ns, r.GetName()))
	if err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "blocks.service.Scale", "read for revision")
	}
	replicas := r.GetReplicas()
	b.Spec.Replicas = &replicas

	// Re-run admission: V5/V7/V16 constraints depend on replica count.
	ad := s.adCtx()
	ad.Existing = s.namesInNamespace(ctx, ns, r.GetName())
	if ves := validate.Validate(b, ad); len(ves) > 0 {
		return nil, admissionErr(ves)
	}
	return s.putVersioned(ctx, "Scale", b, e.Revision)
}

// Restart stamps the restart annotation on every replica (-1/omitted =
// all replicas; per-replica restart rolls into the annotation phase in
// Phase 05). Same txn contract as Update.
func (s *Server) Restart(ctx context.Context, r *pb.RestartRequest) (*pb.Block, error) {
	if r.GetName() == "" {
		return nil, errors.New(errors.KindInvalid, "blocks.service.Restart", "name is required")
	}
	ns := r.GetNamespace()
	if ns == "" {
		ns = "default"
	}
	b, err := s.get(ctx, ns, r.GetName())
	if err != nil {
		return nil, err
	}
	e, err := s.St.Get(ctx, key(ns, r.GetName()))
	if err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "blocks.service.Restart", "read for revision")
	}
	if b.Metadata.Annotations == nil {
		b.Metadata.Annotations = map[string]string{}
	}
	b.Metadata.Annotations[RestartedAtAnnotation] = time.Now().UTC().Format(time.RFC3339)
	return s.putVersioned(ctx, "Restart", b, e.Revision)
}

// putVersioned marshals and CAS-writes a validated block, preserving the
// shared Update txn semantics (status untouched — callers mutated a clone
// of the stored block).
func (s *Server) putVersioned(ctx context.Context, op string, b *pb.Block, rev store.Revision) (*pb.Block, error) {
	k := key(b.GetMetadata().GetNamespace(), b.GetMetadata().GetName())
	gen, err := generationCurrent(ctx, s.St)
	if err != nil {
		return nil, err
	}
	b.Status.ObservedGeneration = int64(gen + 1)
	out, err := proto.Marshal(b)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "blocks.service."+op, "marshal block")
	}
	if _, err := s.St.Txn(ctx, []store.Op{
		{Kind: store.OpCheck, Key: k, Expect: rev},
		{Kind: store.OpPut, Key: k, Value: out},
	}); err != nil {
		return nil, errors.Wrap(err, errors.KindConflict, "blocks.service."+op,
			fmt.Sprintf("block %s/%s modified concurrently",
				b.GetMetadata().GetNamespace(), b.GetMetadata().GetName()))
	}
	return b, nil
}

// Watch streams BlockEvents for the namespace (or one block when
// request.Name is set). It replays nothing — events start at the current
// revision (callers Get first to snapshot, the standard watch idiom).
func (s *Server) Watch(r *pb.WatchBlocksRequest, srv pb.BlockService_WatchServer) error {
	ctx := srv.Context()
	prefix := BlockPrefix
	if r.GetNamespace() != "" {
		prefix += r.GetNamespace() + "/"
	}
	rev, err := generationCurrent(ctx, s.St) // current revision ~= latest write
	if err != nil {
		return err
	}
	events, err := s.St.Watch(ctx, store.Key(prefix), store.Revision(rev))
	if err != nil {
		return errors.Wrap(err, errors.KindInternal, "blocks.service.Watch", "start watch")
	}
	for ev := range events {
		if r.GetName() != "" {
			k := key(r.GetNamespace(), r.GetName())
			if string(ev.Entry.Key) != string(k) {
				continue
			}
		}
		out := &pb.BlockEvent{Type: pb.EventType_EVENT_TYPE_UNSPECIFIED}
		switch {
		case ev.Type == store.EventDelete:
			out.Type = pb.EventType_EVENT_DELETED
		case ev.Prev == nil:
			out.Type = pb.EventType_EVENT_ADDED
		default:
			out.Type = pb.EventType_EVENT_MODIFIED
		}
		if ev.Entry != nil {
			var b pb.Block
			if err := proto.Unmarshal(ev.Entry.Value, &b); err == nil {
				out.Block = &b
			}
		}
		if err := srv.Send(out); err != nil {
			return err
		}
	}
	return nil
}

// ---- CatalogService ----

// CatalogServer implements pb.CatalogServiceServer over the real catalog.
type CatalogServer struct {
	pb.UnimplementedCatalogServiceServer
	Cat *catalog.Catalog
}

// NewCatalogServer builds a CatalogServer.
func NewCatalogServer(c *catalog.Catalog) *CatalogServer {
	return &CatalogServer{Cat: c}
}

// ListTypes returns every loaded block type, sorted by id.
func (s *CatalogServer) ListTypes(ctx context.Context, r *pb.ListTypesRequest) (*pb.ListTypesResponse, error) {
	ids := s.Cat.Types()
	sort.Strings(ids)
	out := &pb.ListTypesResponse{}
	for _, id := range ids {
		t, ok := s.Cat.GetType(id)
		if !ok {
			continue
		}
		out.Types = append(out.Types, toBlockType(t))
	}
	return out, nil
}

// GetType returns one type including its JSON Schema (schema_json).
func (s *CatalogServer) GetType(ctx context.Context, r *pb.GetTypeRequest) (*pb.BlockType, error) {
	if r.GetName() == "" {
		return nil, errors.New(errors.KindInvalid, "catalog.GetType", "type name is required (<category>/<name>)")
	}
	t, ok := s.Cat.GetType(r.GetName())
	if !ok {
		return nil, errors.New(errors.KindNotFound, "catalog.GetType", "unknown type "+r.GetName())
	}
	return toBlockType(t), nil
}

// toBlockType converts a catalog Type to its proto form, rendering the
// compiled JSON Schema back to bytes.
func toBlockType(t *catalog.Type) *pb.BlockType {
	bt := &pb.BlockType{
		Name:         t.ID(),
		Version:      t.Version,
		Description:  t.Description,
		Icon:         t.Icon,
		Capabilities: t.Capabilities,
	}
	if t.Schema != nil {
		if raw, err := json.Marshal(t.Schema); err == nil {
			bt.SchemaJson = raw
		}
	}
	return bt
}
