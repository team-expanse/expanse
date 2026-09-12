package raftstore

import (
	"context"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/hashicorp/raft"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

// LeaderWaitTimeout is how long a node without a quorate leader keeps
// retrying barrier/forward before reporting the cluster read-only (§4.10:
// "no leader after 5 s → degraded read-only"). Package var so tests can
// shorten it.
var LeaderWaitTimeout = 5 * time.Second

// Forwarder forwards a write command from a follower to the current
// leader (§4.1 write forwarding). It must return the same
// (revision, error) semantics as a local apply.
type Forwarder = func(ctx context.Context, c *pb.Command) (store.Revision, error)

// ApplyCommand applies a command through the local Raft log, leader-only,
// with NO forwarding. This is what a forwarded write executes on the
// leader (the ForwardServer calls it). It exists to break the forwarding
// recursion: a stale leader address causes at most one failed hop, never
// a loop.
func (s *Store) ApplyCommand(ctx context.Context, c *pb.Command) (store.Revision, error) {
	if !s.IsLeader() {
		return 0, errors.New(errors.KindUnavailable, "raftstore.ApplyCommand", "not leader")
	}
	b, err := encodeCommand(c)
	if err != nil {
		return 0, err
	}
	f := s.r.Apply(b, applyTimeout)
	if err := f.Error(); err != nil {
		if err == raft.ErrNotLeader {
			return 0, errors.New(errors.KindUnavailable, "raftstore.ApplyCommand", "not leader")
		}
		if err == raft.ErrRaftShutdown {
			return 0, errors.Wrap(err, errors.KindUnavailable, "raftstore.ApplyCommand", "raft unavailable")
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, errors.Wrap(err, errors.KindInternal, "raftstore.ApplyCommand", "raft apply failed")
	}
	res, ok := f.Response().(*applyResult)
	if !ok {
		return 0, errors.New(errors.KindInternal, "raftstore.ApplyCommand", fmt.Sprintf("unexpected apply result %T", f.Response()))
	}
	return res.rev, res.err
}

// ForwardServer implements the InternalStoreService gRPC service: it is
// the leader-side endpoint of write forwarding.
type ForwardServer struct {
	pb.UnimplementedInternalStoreServiceServer
	applier Applier
}

// Applier is the leader-side surface (satisfied by *Store).
type Applier interface {
	ApplyCommand(ctx context.Context, c *pb.Command) (store.Revision, error)
	LinearGet(ctx context.Context, k store.Key) (*store.Entry, error)
	LinearList(ctx context.Context, prefix store.Key) ([]*store.Entry, error)
	LinearRevision(ctx context.Context) (store.Revision, error)
}

// NewForwardServer builds the leader-side forwarding endpoint.
func NewForwardServer(a Applier) *ForwardServer {
	return &ForwardServer{applier: a}
}

// ForwardCommand applies the command and reports the committed revision,
// or the typed apply error (kind/op/message) so the forwarding client can
// reconstruct identical error semantics locally.
func (fs *ForwardServer) ForwardCommand(ctx context.Context, c *pb.Command) (*pb.ForwardCommandResponse, error) {
	rev, err := fs.applier.ApplyCommand(ctx, c)
	if err != nil {
		// Distinguish "I am not the leader (anymore)" — the client should
		// re-resolve — from genuine apply failures, which must be
		// returned as typed details.
		if errors.KindOf(err) == errors.KindUnavailable {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		return &pb.ForwardCommandResponse{
			Result: &pb.ForwardCommandResponse_Error{Error: errorDetail(err)},
		}, nil
	}
	return &pb.ForwardCommandResponse{
		Result: &pb.ForwardCommandResponse_Revision{Revision: uint64(rev)},
	}, nil
}

// LinearRead serves a follower's forwarded linearizable read: barrier
// (leader-side), then read from the FSM.
func (fs *ForwardServer) LinearRead(ctx context.Context, req *pb.LinearReadRequest) (*pb.LinearReadResponse, error) {
	switch q := req.GetQuery().(type) {
	case *pb.LinearReadRequest_GetKey:
		e, err := fs.applier.LinearGet(ctx, store.Key(q.GetKey))
		if err != nil {
			return &pb.LinearReadResponse{Result: &pb.LinearReadResponse_Error{Error: errorDetail(err)}}, nil
		}
		return &pb.LinearReadResponse{Result: &pb.LinearReadResponse_Entry{Entry: entryToPB(e)}}, nil
	case *pb.LinearReadRequest_ListPrefix:
		es, err := fs.applier.LinearList(ctx, store.Key(q.ListPrefix))
		if err != nil {
			return &pb.LinearReadResponse{Result: &pb.LinearReadResponse_Error{Error: errorDetail(err)}}, nil
		}
		out := &pb.EntryList{}
		for _, e := range es {
			out.Entries = append(out.Entries, entryToPB(e))
		}
		return &pb.LinearReadResponse{Result: &pb.LinearReadResponse_List{List: out}}, nil
	case *pb.LinearReadRequest_RevisionOnly:
		rev, err := fs.applier.LinearRevision(ctx)
		if err != nil {
			return &pb.LinearReadResponse{Result: &pb.LinearReadResponse_Error{Error: errorDetail(err)}}, nil
		}
		return &pb.LinearReadResponse{Result: &pb.LinearReadResponse_Revision{Revision: uint64(rev)}}, nil
	default:
		return &pb.LinearReadResponse{Result: &pb.LinearReadResponse_Error{Error: &pb.ErrorDetail{
			Kind: string(errors.KindInvalid), Op: "raftstore.LinearRead", Message: "empty query",
		}}}, nil
	}
}

func entryToPB(e *store.Entry) *pb.Entry {
	return &pb.Entry{
		Key:             string(e.Key),
		Value:           e.Value,
		Revision:        uint64(e.Revision),
		CreatedAtUnixNs: e.CreatedAt,
		UpdatedAtUnixNs: e.UpdatedAt,
	}
}

func entryFromPB(e *pb.Entry) *store.Entry {
	return &store.Entry{
		Key:       store.Key(e.GetKey()),
		Value:     e.GetValue(),
		Revision:  store.Revision(e.GetRevision()),
		CreatedAt: e.GetCreatedAtUnixNs(),
		UpdatedAt: e.GetUpdatedAtUnixNs(),
	}
}

func errorDetail(err error) *pb.ErrorDetail {
	e := &pb.ErrorDetail{}
	switch errors.KindOf(err) {
	case errors.KindNotFound:
		e.Kind = string(errors.KindNotFound)
	case errors.KindConflict:
		e.Kind = string(errors.KindConflict)
	case errors.KindInvalid:
		e.Kind = string(errors.KindInvalid)
	case errors.KindUnavailable:
		e.Kind = string(errors.KindUnavailable)
	case errors.KindPermission:
		e.Kind = string(errors.KindPermission)
	case errors.KindTimeout:
		e.Kind = string(errors.KindTimeout)
	default:
		e.Kind = string(errors.KindInternal)
	}
	var te *errors.Error
	if asTyped(err, &te) {
		e.Op = te.Op
		e.Message = te.Message
	} else {
		e.Message = err.Error()
	}
	return e
}

func asTyped(err error, target **errors.Error) bool {
	for err != nil {
		if e, ok := err.(*errors.Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func errorFromDetail(d *pb.ErrorDetail) error {
	if d == nil {
		return nil
	}
	return errors.New(errors.Kind(d.Kind), d.Op, d.Message)
}

// PeerResolver maps a Raft transport address to that node's internal gRPC
// (forward) address. Wiring supplies this from cluster membership (T08+);
// tests supply a static map.
type PeerResolver func(raftAddr string) (apiAddr string, ok bool)

// GRPCForwarder forwards writes to the leader over gRPC, caching one
// connection per peer. Safe for concurrent use.
type GRPCForwarder struct {
	leaderAddr func() string // current leader's Raft address (empty if none)
	resolve    PeerResolver
	mu         sync.Mutex
	conns      map[string]*grpc.ClientConn
}

// NewGRPCForwarder returns a Forwarder that asks leaderAddr for the
// current leader's Raft address, resolves it to the node's internal gRPC
// endpoint through resolve, and applies the command there. Connections
// are cached and reused; Close releases them.
func NewGRPCForwarder(leaderAddr func() string, resolve PeerResolver) *GRPCForwarder {
	return &GRPCForwarder{
		leaderAddr: leaderAddr,
		resolve:    resolve,
		conns:      make(map[string]*grpc.ClientConn),
	}
}

// Forward implements Forwarder.
func (g *GRPCForwarder) Forward(ctx context.Context, c *pb.Command) (store.Revision, error) {
	raftAddr := g.leaderAddr()
	if raftAddr == "" {
		return 0, errors.New(errors.KindUnavailable, "raftstore.Forward", "no leader known; write dropped (read-only mode)")
	}
	apiAddr, ok := g.resolve(raftAddr)
	if !ok || apiAddr == "" {
		return 0, errors.New(errors.KindUnavailable, "raftstore.Forward", fmt.Sprintf("no internal endpoint for leader %q", raftAddr))
	}
	client, err := g.client(apiAddr)
	if err != nil {
		return 0, errors.Wrap(err, errors.KindUnavailable, "raftstore.Forward", "dial leader")
	}
	resp, err := client.ForwardCommand(ctx, c)
	if err != nil {
		g.invalidate(apiAddr) // stale conn — force redial next time
		if status.Code(err) == codes.Unavailable {
			return 0, errors.New(errors.KindUnavailable, "raftstore.Forward", "leader reported not-leader or unreachable; retry")
		}
		return 0, errors.Wrap(err, errors.KindUnavailable, "raftstore.Forward", "forward rpc failed")
	}
	if e := resp.GetError(); e != nil {
		return 0, errorFromDetail(e)
	}
	return store.Revision(resp.GetRevision()), nil
}

// ReadForwarder forwards linearizable reads from a follower to the
// leader (raft.Barrier only works leader-side).
type ReadForwarder interface {
	ForwardGet(ctx context.Context, k store.Key) (*store.Entry, error)
	ForwardList(ctx context.Context, prefix store.Key) ([]*store.Entry, error)
	ForwardRevision(ctx context.Context) (store.Revision, error)
}

// ForwardGet implements ReadForwarder.
func (g *GRPCForwarder) ForwardGet(ctx context.Context, k store.Key) (*store.Entry, error) {
	resp, err := g.linearRead(ctx, &pb.LinearReadRequest{
		Query: &pb.LinearReadRequest_GetKey{GetKey: string(k)},
	})
	if err != nil {
		return nil, err
	}
	return entryFromPB(resp.GetEntry()), nil
}

// ForwardList implements ReadForwarder.
func (g *GRPCForwarder) ForwardList(ctx context.Context, prefix store.Key) ([]*store.Entry, error) {
	resp, err := g.linearRead(ctx, &pb.LinearReadRequest{
		Query: &pb.LinearReadRequest_ListPrefix{ListPrefix: string(prefix)},
	})
	if err != nil {
		return nil, err
	}
	var out []*store.Entry
	for _, e := range resp.GetList().GetEntries() {
		out = append(out, entryFromPB(e))
	}
	return out, nil
}

// ForwardRevision implements ReadForwarder.
func (g *GRPCForwarder) ForwardRevision(ctx context.Context) (store.Revision, error) {
	resp, err := g.linearRead(ctx, &pb.LinearReadRequest{
		Query: &pb.LinearReadRequest_RevisionOnly{RevisionOnly: true},
	})
	if err != nil {
		return 0, err
	}
	return store.Revision(resp.GetRevision()), nil
}

// linearRead dials the leader (reusing the write path's conn cache) and
// serves one forwarded linearizable read.
func (g *GRPCForwarder) linearRead(ctx context.Context, req *pb.LinearReadRequest) (*pb.LinearReadResponse, error) {
	raftAddr := g.leaderAddr()
	if raftAddr == "" {
		return nil, errors.New(errors.KindUnavailable, "raftstore.ForwardRead", "no leader known; node is read-only (degraded)")
	}
	apiAddr, ok := g.resolve(raftAddr)
	if !ok || apiAddr == "" {
		return nil, errors.New(errors.KindUnavailable, "raftstore.ForwardRead", fmt.Sprintf("no internal endpoint for leader %q", raftAddr))
	}
	client, err := g.client(apiAddr)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindUnavailable, "raftstore.ForwardRead", "dial leader")
	}
	resp, err := client.LinearRead(ctx, req)
	if err != nil {
		g.invalidate(apiAddr)
		if status.Code(err) == codes.Unavailable {
			return nil, errors.New(errors.KindUnavailable, "raftstore.ForwardRead", "leader unreachable; retry")
		}
		return nil, errors.Wrap(err, errors.KindUnavailable, "raftstore.ForwardRead", "linear read rpc failed")
	}
	if e := resp.GetError(); e != nil {
		return nil, errorFromDetail(e)
	}
	return resp, nil
}

// Close releases all cached peer connections.
func (g *GRPCForwarder) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	var firstErr error
	for a, c := range g.conns {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(g.conns, a)
	}
	return firstErr
}

func (g *GRPCForwarder) client(apiAddr string) (pb.InternalStoreServiceClient, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if conn, ok := g.conns[apiAddr]; ok {
		return pb.NewInternalStoreServiceClient(conn), nil
	}
	// Loopback-plaintext for now: mTLS lands with the join service (T10).
	conn, err := grpc.NewClient(apiAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	g.conns[apiAddr] = conn
	return pb.NewInternalStoreServiceClient(conn), nil
}

func (g *GRPCForwarder) invalidate(apiAddr string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if conn, ok := g.conns[apiAddr]; ok {
		_ = conn.Close()
		delete(g.conns, apiAddr)
	}
}
