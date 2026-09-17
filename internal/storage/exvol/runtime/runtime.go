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
			if status.Primary == r.opts.NodeID && status.State != storage.StateDeleting {
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
		return err
	}
	var reps []primary.Replica
	for _, p := range status.Placement {
		if p.NodeID == r.opts.NodeID {
			continue
		}
		conn, err := r.dialReplica(ctx, p.NodeID)
		if err != nil {
			w.Close()
			return err
		}
		reps = append(reps, primary.Replica{NodeID: p.NodeID, Sender: transport.NewSender(conn, 0, 0)})
	}
	coord := primary.New(spec.ID, spec.Replication, w, reps, held, 5*time.Second)
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
	r.prim[spec.ID] = &volPrimary{coord: coord, dev: dev, nsrv: nsrv, held: held, writer: w}
	r.mu.Unlock()
	r.opts.Logger.Info("primary serving", "vol", spec.ID, "zvol", zvolPath, "replicas", len(reps))
	return nil
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
			delete(r.prim, id)
		}
	}
}
