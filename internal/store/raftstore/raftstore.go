package raftstore

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

// Snapshot policy (spec §10 "Raft log growing unbounded"): snapshot every
// 8192 committed entries. The 64 MB alternative bound is approximated by
// the entry threshold; a 64 MB log is roughly 8k entries at realistic
// command sizes, so the entry count alone is an adequate proxy.
const (
	snapshotThreshold = 8192
	snapshotInterval  = 30 * time.Second
	applyTimeout      = 10 * time.Second
	barrierTimeout    = 10 * time.Second
)

// Config configures a raftstore node.
type Config struct {
	NodeID        string        // Raft server ID (required)
	BindAddr      string        // Raft transport listen address (required, host:port)
	AdvertiseAddr string        // advertised transport address (default: BindAddr); set to IP:7444 when binding 0.0.0.0
	DataDir       string        // persistent state dir (required); raft files under <DataDir>/raft
	Bootstrap     bool          // bootstrap a brand-new single-node cluster
	Logger        raft.LogStore // unused placeholder (kept nil); logging goes to stderr
}

// Store is the Raft-replicated store.Store.
type Store struct {
	cfg       Config
	r         *raft.Raft
	fsm       *FSM
	trans     *raft.NetworkTransport
	logs      io.Closer     // bolt log+stable store; closed after raft shutdown
	forwarder Forwarder     // leader write-forwarding hook (set by daemon wiring)
	readFwd   ReadForwarder // leader read-forwarding hook (linearizable reads)
}

// Open creates (or rejoins) a raftstore node. With Bootstrap=true a fresh
// single-voter cluster is formed; the caller must ensure exactly one node
// bootstraps (cluster init does).
func Open(cfg Config) (*Store, error) {
	if cfg.NodeID == "" || cfg.BindAddr == "" || cfg.DataDir == "" {
		return nil, errors.New(errors.KindInvalid, "raftstore.Open", "NodeID, BindAddr and DataDir are required")
	}
	raftDir := filepath.Join(cfg.DataDir, "raft")
	if err := os.MkdirAll(filepath.Join(raftDir, "snapshots"), 0o750); err != nil {
		return nil, fmt.Errorf("create raft dir: %w", err)
	}

	addr, err := net.ResolveTCPAddr("tcp", cfg.BindAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve bind addr: %w", err)
	}
	advAddr := addr
	if cfg.AdvertiseAddr != "" {
		advAddr, err = net.ResolveTCPAddr("tcp", cfg.AdvertiseAddr)
		if err != nil {
			return nil, fmt.Errorf("resolve advertise addr: %w", err)
		}
	}
	trans, err := raft.NewTCPTransport(cfg.BindAddr, advAddr, 3, 10*time.Second, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("raft transport: %w", err)
	}

	logStore, err := raftboltdb.NewBoltStore(filepath.Join(raftDir, "raft.db"))
	if err != nil {
		_ = trans.Close()
		return nil, fmt.Errorf("bolt log store: %w", err)
	}
	// raft-boltdb v2 implements both LogStore and StableStore.
	stableStore := logStore

	snapshots, err := raft.NewFileSnapshotStore(filepath.Join(raftDir, "snapshots"), 3, os.Stderr)
	if err != nil {
		_ = logStore.Close()
		_ = trans.Close()
		return nil, fmt.Errorf("snapshot store: %w", err)
	}

	fsm := NewFSM()
	rc := raft.DefaultConfig()
	rc.LocalID = raft.ServerID(cfg.NodeID)
	rc.HeartbeatTimeout = 1000 * time.Millisecond
	rc.ElectionTimeout = 1000 * time.Millisecond
	rc.CommitTimeout = 50 * time.Millisecond // batch commits for throughput
	rc.SnapshotThreshold = snapshotThreshold
	rc.SnapshotInterval = snapshotInterval
	rc.LeaderLeaseTimeout = 500 * time.Millisecond
	rc.MaxAppendEntries = 256
	rc.LogLevel = "ERROR"
	rc.LogOutput = io.Discard
	r, err := raft.NewRaft(rc, fsm, logStore, stableStore, snapshots, trans)
	if err != nil {
		_ = logStore.Close()
		_ = trans.Close()
		return nil, fmt.Errorf("new raft: %w", err)
	}

	s := &Store{cfg: cfg, r: r, fsm: fsm, trans: trans, logs: logStore}

	if cfg.Bootstrap {
		// Only bootstrap when the store is empty; a restarted node must
		// rejoin its existing log instead of clobbering it.
		hasState, err := raft.HasExistingState(logStore, stableStore, snapshots)
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("check existing state: %w", err)
		}
		if !hasState {
			bc := raft.Configuration{
				Servers: []raft.Server{{
					ID:      raft.ServerID(cfg.NodeID),
					Address: trans.LocalAddr(),
				}},
			}
			if err := r.BootstrapCluster(bc).Error(); err != nil {
				s.Close()
				return nil, fmt.Errorf("bootstrap cluster: %w", err)
			}
		}
	}
	return s, nil
}

// Leader reports the current leader's transport address, or "" if unknown.
func (s *Store) Leader() string {
	return string(s.r.Leader())
}

// NodeID reports this node's ID.
func (s *Store) NodeID() string { return s.cfg.NodeID }

// State reports the node's Raft role.
func (s *Store) State() raft.RaftState { return s.r.State() }

// IsLeader reports whether this node currently leads.
func (s *Store) IsLeader() bool { return s.r.State() == raft.Leader }

// --- store.Store ---

// staleFrom reports whether ctx opted into stale (local-FSM) reads via
// store.WithStale. Linearizable is the default: one Barrier round trip
// before serving. Scheduling and lease decisions must NEVER use stale
// reads (see AssertNoStaleReads in internal/store).
func staleFrom(ctx context.Context) bool { return store.StaleFrom(ctx) }

// barrier waits until this node's FSM reflects everything committed as of
// now (linearizable read barrier). Returns an error of kind
// KindUnavailable when no leader is reachable within LeaderWaitTimeout.
func (s *Store) barrier(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, LeaderWaitTimeout)
	defer cancel()
	for {
		err := s.r.Barrier(barrierTimeout).Error()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-cctx.Done():
			return errors.New(errors.KindUnavailable, "raftstore.barrier",
				"no leader reachable; node is read-only (degraded)")
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Get returns the entry for k. Linearizable by default; store.WithStale
// serves from the local FSM with no round trip. On a follower a
// linearizable read is forwarded to the leader (raft.Barrier is
// leader-only).
func (s *Store) Get(ctx context.Context, k store.Key) (*store.Entry, error) {
	if staleFrom(ctx) {
		return s.fsm.get(k)
	}
	if !s.IsLeader() && s.readFwd != nil {
		return s.readFwd.ForwardGet(ctx, k)
	}
	if err := s.barrier(ctx); err != nil {
		return nil, err
	}
	return s.fsm.get(k)
}

// List returns all entries under prefix, sorted by key.
func (s *Store) List(ctx context.Context, prefix store.Key) ([]*store.Entry, error) {
	if staleFrom(ctx) {
		return s.fsm.list(prefix)
	}
	if !s.IsLeader() && s.readFwd != nil {
		return s.readFwd.ForwardList(ctx, prefix)
	}
	if err := s.barrier(ctx); err != nil {
		return nil, err
	}
	return s.fsm.list(prefix)
}

// Put writes k→v unconditionally. Encoded as a single-op txn (CmdPut is
// reserved for conditional CAS semantics).
func (s *Store) Put(ctx context.Context, k store.Key, v []byte) (store.Revision, error) {
	return s.Txn(ctx, []store.Op{{Kind: store.OpPut, Key: k, Value: v}})
}

// CompareAndSwap writes v only if k's revision equals expect.
func (s *Store) CompareAndSwap(ctx context.Context, k store.Key, expect store.Revision, v []byte) (store.Revision, error) {
	c := &pb.Command{
		Type:            pb.CommandType_COMMAND_TYPE_PUT,
		Key:             string(k),
		Value:           v,
		Expect:          uint64(expect),
		TimestampUnixNs: time.Now().UnixNano(), // set by the leader, NOT by the FSM
	}
	return s.propose(ctx, c)
}

// Delete removes k, optionally checking its revision.
func (s *Store) Delete(ctx context.Context, k store.Key, expect store.Revision) error {
	c := &pb.Command{
		Type:            pb.CommandType_COMMAND_TYPE_DELETE,
		Key:             string(k),
		Expect:          uint64(expect),
		TimestampUnixNs: time.Now().UnixNano(),
	}
	_, err := s.propose(ctx, c)
	return err
}

// Txn applies ops atomically.
func (s *Store) Txn(ctx context.Context, ops []store.Op) (store.Revision, error) {
	c := &pb.Command{
		Type:            pb.CommandType_COMMAND_TYPE_TXN,
		Ops:             protoOps(ops),
		TimestampUnixNs: time.Now().UnixNano(),
	}
	return s.propose(ctx, c)
}

// propose applies a command through Raft. Followers forward the write to
// the leader (write forwarding, §4.1) via s.Forwarder when configured;
// otherwise — or when forwarding fails — the caller gets
// KindUnavailable.
func (s *Store) propose(ctx context.Context, c *pb.Command) (store.Revision, error) {
	if fwd := s.forwarder; fwd != nil && !s.IsLeader() {
		return fwd(ctx, c)
	}
	b, err := encodeCommand(c)
	if err != nil {
		return 0, err
	}
	f := s.r.Apply(b, applyTimeout)
	if err := f.Error(); err != nil {
		if err == raft.ErrNotLeader {
			if fwd := s.forwarder; fwd != nil {
				return fwd(ctx, c)
			}
			return 0, errors.New(errors.KindUnavailable, "raftstore.propose", "not leader")
		}
		return 0, errors.Wrap(err, errors.KindUnavailable, "raftstore.propose", "raft apply failed")
	}
	res, ok := f.Response().(*applyResult)
	if !ok {
		return 0, errors.New(errors.KindInternal, "raftstore.propose", fmt.Sprintf("unexpected apply result %T", f.Response()))
	}
	return res.rev, res.err
}

// Forwarder is installed by the daemon wiring — see forward.go.
//
// SetForwarder installs the leader write-forwarding hook; SetReadForwarder
// installs the linearizable read-forwarding hook (followers forward reads
// — raft.Barrier is leader-only).
func (s *Store) SetForwarder(f Forwarder)          { s.forwarder = f }
func (s *Store) SetReadForwarder(rf ReadForwarder) { s.readFwd = rf }

// Watch registers a watcher under prefix (local FSM events, in revision
// order, same semantics as boltstore).
func (s *Store) Watch(ctx context.Context, prefix store.Key, fromRev store.Revision) (<-chan store.Event, error) {
	return s.fsm.watchers.watch(ctx, prefix, fromRev), nil
}

// Revision returns the current revision (linearizable by default).
func (s *Store) Revision(ctx context.Context) (store.Revision, error) {
	if staleFrom(ctx) {
		return s.fsm.Revision(), nil
	}
	if !s.IsLeader() && s.readFwd != nil {
		return s.readFwd.ForwardRevision(ctx)
	}
	if err := s.barrier(ctx); err != nil {
		return 0, err
	}
	return s.fsm.Revision(), nil
}

// LinearGet is the leader-side endpoint of forwarded linearizable reads.
func (s *Store) LinearGet(ctx context.Context, k store.Key) (*store.Entry, error) {
	if !s.IsLeader() {
		return nil, errors.New(errors.KindUnavailable, "raftstore.LinearGet", "not leader")
	}
	if err := s.barrier(ctx); err != nil {
		return nil, err
	}
	return s.fsm.get(k)
}

// LinearList is the leader-side endpoint of forwarded linearizable reads.
func (s *Store) LinearList(ctx context.Context, prefix store.Key) ([]*store.Entry, error) {
	if !s.IsLeader() {
		return nil, errors.New(errors.KindUnavailable, "raftstore.LinearList", "not leader")
	}
	if err := s.barrier(ctx); err != nil {
		return nil, err
	}
	return s.fsm.list(prefix)
}

// LinearRevision is the leader-side endpoint of forwarded linearizable reads.
func (s *Store) LinearRevision(ctx context.Context) (store.Revision, error) {
	if !s.IsLeader() {
		return 0, errors.New(errors.KindUnavailable, "raftstore.LinearRevision", "not leader")
	}
	if err := s.barrier(ctx); err != nil {
		return 0, err
	}
	return s.fsm.Revision(), nil
}

// StateHash returns the sha256 of the canonical local state — used by the
// chaos suite's divergence checker.
func (s *Store) StateHash() [32]byte { return s.fsm.StateHash() }

// Snapshot manually triggers a Raft snapshot (used by `expanse ctl raft
// snapshot` and tests).
func (s *Store) Snapshot() error { return s.r.Snapshot().Error() }

// AddVoter adds a server to the cluster (join flow).
func (s *Store) AddVoter(id, addr string) error {
	f := s.r.AddVoter(raft.ServerID(id), raft.ServerAddress(addr), 0, applyTimeout)
	return f.Error()
}

// RemoveServer removes a server from the cluster (node removal flow).
func (s *Store) RemoveServer(id string) error {
	f := s.r.RemoveServer(raft.ServerID(id), 0, applyTimeout)
	return f.Error()
}

// TransferLeadership attempts to move leadership to id.
func (s *Store) TransferLeadership(id string) error {
	return s.r.LeadershipTransferToServer(raft.ServerID(id), raft.ServerAddress(id)).Error()
}

// Close shuts down Raft and releases resources. Safe to call once.
func (s *Store) Close() error {
	f := s.r.Shutdown()
	if err := f.Error(); err != nil {
		return fmt.Errorf("raft shutdown: %w", err)
	}
	if err := s.trans.Close(); err != nil {
		return fmt.Errorf("transport close: %w", err)
	}
	// The bolt store must be closed AFTER raft shutdown — closing it
	// first would leave raft writing to a closed DB (panics).
	if err := s.logs.Close(); err != nil {
		return fmt.Errorf("log store close: %w", err)
	}
	s.fsm.watchers.close()
	return nil
}
