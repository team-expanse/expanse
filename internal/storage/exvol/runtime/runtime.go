// Package runtime is the node-local exvol volume runtime (Phase 06
// T10 — the minimal end-to-end wiring; T13's full controller grows out
// of this). It reconciles the node's share of the volume state in the
// cluster store:
//
//   - every replica: ensure the zvol exists in the pool
//     (<pool>/volumes/<id>), and serve it on the shared replication
//     transport (secondary role);
//   - the primary: acquire the volume lease (§4.3 R1 — writes require
//     a valid lease), run the T07 coordinator against the other
//     replicas over the T05 transport, and attach the T09 device —
//     NBD server + nbd-client, /dev/exvol/<id> appearing when done.
//
// Creation is leader-side via `expanse ctl volume create` (writes the
// spec + initial status records; the per-node runtimes converge).
package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	pbproto "google.golang.org/protobuf/proto"

	"github.com/expanse/expanse/internal/cluster/lease"
	"github.com/expanse/expanse/internal/config"
	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/storage/exvol/device"
	"github.com/expanse/expanse/internal/storage/exvol/localwrite"
	"github.com/expanse/expanse/internal/storage/exvol/primary"
	"github.com/expanse/expanse/internal/storage/exvol/protocol"
	"github.com/expanse/expanse/internal/storage/exvol/recovery"
	"github.com/expanse/expanse/internal/storage/exvol/secondary"
	"github.com/expanse/expanse/internal/storage/exvol/transport"
	"github.com/expanse/expanse/internal/storage/zfs"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

// Options configures a node's volume runtime.
type Options struct {
	NodeID  string
	St      store.Store
	Pool    string // zpool holding the volumes/<id> zvols
	DataDir string // NBD socket directory
	Logger  *slog.Logger

	// AddrOf resolves a node ID to its mesh (exp0) address. The agent
	// wires this from each node's published overlay prefix.
	AddrOf func(nodeID string) (string, error)

	// Port is the replication-transport listen port (default 9440).
	Port int

	// LeaseTTL is the volume-primary lease TTL (default 10s).
	LeaseTTL time.Duration

	// IsLeader reports whether THIS node holds raft leadership. Only
	// the leader processes pending volume-creation requests.
	IsLeader func() bool
}

// Runtime is one node's volume runtime.
type Runtime struct {
	opts Options
	zfs  *zfs.Exec
	lm   *lease.Manager

	mu       sync.Mutex
	srv      *transport.Server // shared per-node replication listener
	secs     map[string]*secondary.Secondary
	prim     map[string]*volPrimary
	nbdSlots map[string]string // volID → /dev/nbdN (assigned once)
}

type volPrimary struct {
	coord  *primary.Coordinator
	dev    *device.ExvolDevice
	nsrv   *device.Server
	held   *lease.Held
	writer *localwrite.Writer
	conns  map[string]*transport.Conn
}

// New builds the runtime. Call Run in a goroutine.
func New(opts Options) *Runtime {
	if opts.Port == 0 {
		opts.Port = config.PortExvol
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = 10 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Runtime{
		opts:     opts,
		zfs:      zfs.New(),
		lm:       lease.NewManager(opts.St, opts.NodeID),
		secs:     map[string]*secondary.Secondary{},
		prim:     map[string]*volPrimary{},
		nbdSlots: map[string]string{},
	}
}

// Run reconciles every 2s until the context is done.
func (r *Runtime) Run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		if err := r.Reconcile(ctx); err != nil {
			r.opts.Logger.Warn("volume reconcile failed", "err", err)
		}
		select {
		case <-ctx.Done():
			r.stopAll()
			return
		case <-t.C:
		}
	}
}

func (r *Runtime) stopAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, p := range r.prim {
		p.nsrv.Close()
		p.coord = nil
		p.held.Abandon()
		p.writer.Close()
		for _, cn := range p.conns {
			cn.Close()
		}
		delete(r.prim, id)
	}
	r.secs = map[string]*secondary.Secondary{}
	if r.srv != nil {
		r.srv.Close()
		r.srv = nil
	}
}

// Reconcile converges local state with the store once.
func (r *Runtime) Reconcile(ctx context.Context) error {
	if err := r.processPendingCreates(ctx); err != nil {
		r.opts.Logger.Warn("pending volume create failed", "err", err)
	}
	ids, err := storage.ListVolumeIDs(ctx, r.opts.St)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, id := range ids {
		spec, err := storage.LoadSpec(ctx, r.opts.St, id)
		if err != nil {
			continue // not yet written / deleted mid-flight
		}
		status, _, err := storage.LoadStatus(ctx, r.opts.St, id)
		if err != nil {
			continue
		}
		seen[id] = true
		mine, zvolPath := r.myReplica(spec.ID, status)
		if mine {
			devNode, err := r.ensureZvol(ctx, spec, zvolPath)
			if err != nil {
				r.opts.Logger.Warn("zvol ensure failed", "vol", id, "err", err)
				continue
			}
			if status.Primary == r.opts.NodeID && status.State != storage.StateDeleting &&
				status.State != storage.StateNeedsManualRecovery {
				if err := r.ensurePrimary(ctx, spec, status, zvolPath, devNode); err != nil {
					r.opts.Logger.Warn("primary ensure failed", "vol", id, "err", err)
				}
			}
			if status.Primary != r.opts.NodeID {
				r.ensureSecondary(spec.ID, spec, zvolPath, devNode)
			}
		}
	}
	r.pruneNotIn(seen)
	return nil
}

// processPendingCreates performs placement for pending volume-creation
// requests (leader only). Idempotent: it deletes the request after
// writing spec + status; a crash between the two is healed by the next
// pass re-placing (spec IDs are random, so the stale partial spec is
// pruned by T13's garbage collection — creation is retried by the user
// meanwhile).
func (r *Runtime) processPendingCreates(ctx context.Context) error {
	if r.opts.IsLeader == nil || !r.opts.IsLeader() {
		return nil
	}
	entries, err := r.opts.St.List(ctx, storage.PendingPrefix)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := strings.TrimPrefix(string(e.Key), storage.PendingPrefix)
		var req pb.VolumeSpec
		if pbproto.Unmarshal(e.Value, &req) != nil || req.GetName() == "" {
			// Undecodable request: drop it so it can't wedge the loop.
			r.opts.St.Delete(ctx, e.Key, 0)
			continue
		}
		if err := r.placeVolume(ctx, &req); err != nil {
			r.opts.Logger.Warn("volume placement failed", "vol", name, "err", err)
			continue // retried next tick
		}
		r.opts.St.Delete(ctx, e.Key, 0)
		r.opts.Logger.Info("volume placed", "vol", name)
	}
	return nil
}

// placeVolume selects replica nodes and writes spec + initial status.
func (r *Runtime) placeVolume(ctx context.Context, req *pb.VolumeSpec) error {
	if _, err := storage.LoadSpec(ctx, r.opts.St, req.GetName()); err == nil {
		return nil // already placed (retry after a crash mid-delete)
	}
	// Candidates: nodes with a published mesh record (enrolled + meshed).
	peers, err := r.opts.St.List(ctx, "/nodes/")
	if err != nil {
		return err
	}
	var infos []storage.NodeInfo
	for _, pe := range peers {
		if !strings.HasSuffix(string(pe.Key), "/network.wgPublicKey") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(string(pe.Key), "/nodes/"), "/network.wgPublicKey")
		infos = append(infos, storage.NodeInfo{ID: id, PoolName: r.opts.Pool})
	}
	if len(infos) == 0 {
		return experrors.New(experrors.KindUnavailable, "exvol.runtime.place", "no meshed nodes to place replicas on")
	}
	class := storage.DefaultStorageClass()
	class.Replication = int(req.GetReplication())
	chosen, err := storage.SelectNodes(class, infos, nil)
	if err != nil {
		return err
	}
	id := "vol-" + randHex()
	spec := storage.Spec{
		ID:          id,
		Name:        req.GetName(),
		Namespace:   "default",
		SizeBytes:   req.GetSizeBytes(),
		Class:       req.GetClass(),
		Replication: int(req.GetReplication()),
	}
	status := storage.Status{State: storage.StateHealthy, Primary: chosen[0].ID}
	for i, n := range chosen {
		role := storage.RoleSecondary
		if i == 0 {
			role = storage.RolePrimary
		}
		status.Placement = append(status.Placement, storage.Replica{
			NodeID: n.ID, Role: role,
			ZvolPath: fmt.Sprintf("%s/volumes/%s", r.opts.Pool, id),
			Healthy:  true,
		})
	}
	if err := storage.SaveSpec(ctx, r.opts.St, spec); err != nil {
		return err
	}
	return storage.SaveStatus(ctx, r.opts.St, id, status)
}

func randHex() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on Linux
	}
	return hex.EncodeToString(b)
}

// myReplica returns whether this node holds a replica and its zvol path.
func (r *Runtime) myReplica(volID string, status storage.Status) (bool, string) {
	for _, p := range status.Placement {
		if p.NodeID == r.opts.NodeID {
			if p.ZvolPath != "" {
				return true, p.ZvolPath
			}
			return true, fmt.Sprintf("%s/volumes/%s", r.opts.Pool, volID)
		}
	}
	return false, ""
}

// ensureZvol makes sure the zvol exists and returns its block device
// node (/dev/zvol/<dataset> — the udev link OpenZFS ships; the older
// /dev/<pool>/<ds> tree was removed in OpenZFS 2.4).
func (r *Runtime) ensureZvol(ctx context.Context, spec storage.Spec, zvolPath string) (string, error) {
	dev := "/dev/zvol/" + zvolPath
	if _, err := os.Stat(dev); err == nil {
		return dev, nil
	}
	if err := r.zfs.CreateZvol(ctx, zvolPath, spec.SizeBytes, map[string]string{
		"refreservation": "none", // sparse; reservations are §4.6 (T13) accounting
	}); err != nil {
		// The dataset may exist from a previous tick (created before
		// udev published the device link). Re-stat to decide.
		if _, statErr := os.Stat(dev); statErr != nil {
			return "", err
		}
	}
	r.opts.Logger.Info("zvol created", "zvol", zvolPath, "size", spec.SizeBytes)
	return dev, nil
}

// ensureSecondary registers the volume's secondary on the shared
// replication server (starting the server on first use).
func (r *Runtime) ensureSecondary(volID string, spec storage.Spec, zvolPath, devNode string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.secs[volID]; ok {
		return
	}
	if err := r.ensureServerLocked(); err != nil {
		r.opts.Logger.Warn("replication server unavailable", "err", err)
		return
	}
	w, err := localwrite.Open(devNode, int64(spec.SizeBytes))
	if err != nil {
		r.opts.Logger.Warn("local writer open failed", "vol", volID, "err", err)
		return
	}
	r.secs[volID] = secondary.New(r.opts.NodeID, int(spec.SizeBytes), w)
	r.opts.Logger.Info("secondary serving", "vol", volID, "zvol", zvolPath)
}

func (r *Runtime) ensureServerLocked() error {
	if r.srv != nil {
		return nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", r.opts.Port))
	if err != nil {
		return err
	}
	r.srv = transport.NewServer(ln, func(volID string) (transport.Handler, error) {
		sec, ok := r.secs[volID]
		if !ok {
			return nil, experrors.New(experrors.KindNotFound, "exvol.runtime", "no such volume: "+volID)
		}
		return sec.Handler(), nil
	}, nil, 0)
	go r.srv.Serve() //nolint:errcheck — Close during shutdown is fine
	return nil
}

// ensurePrimary acquires the volume lease and stands up the write path:
// coordinator + device + NBD attach. Lease contention (an old primary
// still holding) just means a retry next tick.
func (r *Runtime) ensurePrimary(ctx context.Context, spec storage.Spec, status storage.Status, zvolPath, devNode string) error {
	r.mu.Lock()
	_, have := r.prim[spec.ID]
	r.mu.Unlock()
	if have {
		return nil
	}

	held, err := r.lm.TryAcquire(ctx, "exvol-vol-"+spec.ID, r.opts.LeaseTTL)
	if err != nil {
		return fmt.Errorf("volume lease held by another primary: %w", err)
	}

	w, err := localwrite.Open(devNode, int64(spec.SizeBytes))
	if err != nil {
		r.lm.Release(ctx, held)
		return err
	}
	// Dial every other replica once; the recovery probe and the write
	// fan-out share the connections.
	conns := map[string]*transport.Conn{}
	for _, p := range status.Placement {
		if p.NodeID == r.opts.NodeID {
			continue
		}
		conn, err := r.dialReplica(ctx, p.NodeID)
		if err != nil {
			w.Close()
			r.lm.Release(ctx, held)
			return err
		}
		conns[p.NodeID] = conn
	}

	// §4.3 recovery: a volume with prior writes resumes only after the
	// failover algorithm has run (4a probes, 4b pulls, 4c leveling,
	// 4d stale marks). Sequence 0 = freshly created, nothing to recover.
	startSeq := status.Sequence
	if startSeq > 0 {
		startSeq, err = r.recoverVol(ctx, spec.ID, w, conns)
		if err != nil {
			w.Close()
			r.lm.Release(ctx, held)
			for _, cn := range conns {
				cn.Close()
			}
			var div *recovery.DivergedError
			if errors.As(err, &div) {
				// §9: refuse automatic recovery, preserve all copies,
				// hand the decision to an operator.
				r.markDiverged(ctx, spec.ID)
				return err
			}
			return fmt.Errorf("volume recovery failed: %w", err)
		}
	}
	var reps []primary.Replica
	for nodeID, conn := range conns {
		reps = append(reps, primary.Replica{NodeID: nodeID, Sender: transport.NewSender(conn, 0, 0)})
	}
	coord := primary.NewAt(spec.ID, spec.Replication, w, reps, held, 5*time.Second, startSeq)
	dev := device.New(spec.ID, int64(spec.SizeBytes), w, coord, held)

	sock := fmt.Sprintf("%s/nbd-%s.sock", r.opts.DataDir, spec.ID)
	nsrv, err := device.Listen(sock)
	if err != nil {
		w.Close()
		r.lm.Release(ctx, held)
		return err
	}
	nsrv.SetDevice(dev)
	go nsrv.Serve() //nolint:errcheck
	if err := device.Attach(sock, spec.ID, r.nbdDevFor(spec.ID), device.ExecRunner{}); err != nil {
		nsrv.Close()
		w.Close()
		r.lm.Release(ctx, held) // free the lease; the next tick retries
		return err
	}

	r.mu.Lock()
	r.prim[spec.ID] = &volPrimary{coord: coord, dev: dev, nsrv: nsrv, held: held, writer: w, conns: conns}
	r.mu.Unlock()
	r.opts.Logger.Info("primary serving", "vol", spec.ID, "zvol", zvolPath, "replicas", len(reps), "seq", startSeq)
	return nil
}

// recoverVol runs the §4.3 failover recovery algorithm (4a–4d) against
// the other replicas over their live transport connections, returning
// the seq the new primary resumes at (step 5). It returns a
// *recovery.DivergedError when the branches have diverged (§9) — the
// caller must NOT serve and must preserve all copies.
func (r *Runtime) recoverVol(ctx context.Context, volID string, w *localwrite.Writer, conns map[string]*transport.Conn) (uint64, error) {
	probes := make([]recovery.Probe, 0, len(conns)+1)
	for nodeID, conn := range conns {
		nodeID, conn := nodeID, conn
		qrep, err := conn.QuerySeq(volID) // step 4a
		if err != nil {
			// Unreachable mid-recovery: step 4d marks it Stale.
			probes = append(probes, recovery.Probe{NodeID: nodeID, Reachable: false})
			continue
		}
		crcs := map[uint64]uint32{}
		for _, op := range qrep.GetOps() {
			crcs[op.GetSeq()] = op.GetCrc32C()
		}
		probes = append(probes, recovery.Probe{
			NodeID:    nodeID,
			Reachable: true,
			LastSeq:   qrep.GetLastSeq(),
			CRCs:      crcs,
			FetchOps: func(_ context.Context, from, to uint64) ([]protocol.WriteOp, error) {
				frep, err := conn.FetchOps(volID, from, to)
				if err != nil {
					return nil, err
				}
				var ops []protocol.WriteOp
				for _, req := range frep.GetOps() {
					ops = append(ops, protocol.WriteOp{
						Seq: req.GetSeq(), Offset: req.GetOffset(),
						Data: req.GetData(), CRC: req.GetCrc32C(), Flush: req.GetFlush(),
					})
				}
				return ops, nil
			},
		})
	}
	res, err := recovery.Recover(ctx, probes,
		func(_ context.Context, op protocol.WriteOp) error { // 4b
			if op.Flush {
				return w.Flush()
			}
			return w.WriteAt(op.Data, int64(op.Offset))
		},
		func(_ context.Context, target string, op protocol.WriteOp) error { // 4c
			conn := conns[target]
			if conn == nil {
				return fmt.Errorf("no connection to %s", target)
			}
			if err := conn.Send(volID, op); err != nil {
				return err
			}
			rep, err := conn.Recv()
			if err != nil {
				return err
			}
			if !rep.ACK {
				return fmt.Errorf("replica %s nacked recovery op %d: %s", target, op.Seq, rep.Reason)
			}
			return nil
		})
	if err != nil {
		return 0, err
	}
	r.opts.Logger.Info("volume recovered", "vol", volID,
		"primary", res.NewPrimaryID, "seq", res.MaxSeq, "pulled", res.Pulled, "stale", res.Stale)
	return res.MaxSeq, nil
}

// markDiverged records §9's NeedsManualRecovery state — automatic
// recovery is refused and every copy is left exactly as found.
func (r *Runtime) markDiverged(ctx context.Context, volID string) {
	status, rev, err := storage.LoadStatus(ctx, r.opts.St, volID)
	if err != nil {
		return
	}
	status.State = storage.StateNeedsManualRecovery
	_ = storage.CompareAndSwapStatus(ctx, r.opts.St, volID, rev, status)
	r.opts.Logger.Error("volume diverged — manual recovery required, all copies preserved", "vol", volID)
}

// dialReplica connects to a node's replication transport with retries —
// the peer's secondary comes up on its own reconcile tick, so the
// primary may be ready before the secondaries are.
func (r *Runtime) dialReplica(ctx context.Context, nodeID string) (*transport.Conn, error) {
	addr, err := r.opts.AddrOf(nodeID)
	if err != nil {
		return nil, err
	}
	target := fmt.Sprintf("%s:%d", addr, r.opts.Port)
	var last error
	for i := 0; i < 30; i++ {
		cn, derr := transport.Dial(ctx, target)
		if derr == nil {
			return cn, nil
		}
		last = derr
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, last
}

// nbdDevFor assigns a stable /dev/nbdN slot per volume (in-process).
func (r *Runtime) nbdDevFor(volID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d, ok := r.nbdSlots[volID]; ok {
		return d
	}
	d := fmt.Sprintf("/dev/nbd%d", len(r.nbdSlots))
	r.nbdSlots[volID] = d
	return d
}

// pruneNotIn stops local roles for volumes that disappeared from the
// store (delete handling stays minimal in T10; T13 grows the rest).
func (r *Runtime) pruneNotIn(seen map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.secs {
		if !seen[id] {
			delete(r.secs, id)
		}
	}
	for id, p := range r.prim {
		if !seen[id] {
			p.nsrv.Close()
			p.held.Abandon()
			p.writer.Close()
			for _, cn := range p.conns {
				cn.Close()
			}
			delete(r.prim, id)
		}
	}
}
