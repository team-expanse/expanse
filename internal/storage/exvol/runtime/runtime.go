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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
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
	"github.com/expanse/expanse/internal/storage/exvol/resync"
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

	// SnapshotInterval is the periodic @resync-<seq> cadence (§4.3,
	// G6.6; default 60 s).
	SnapshotInterval time.Duration

	// SnapshotKeep is the snapshot retention (default 10).
	SnapshotKeep int

	// ResyncBytesPerSec rate-limits resync streams (default 100 MiB/s).
	ResyncBytesPerSec int64

	// ZvolDevBase prefixes the zvol device path (default "/dev/zvol";
	// tests point this at a directory of regular files).
	ZvolDevBase string

	// ZFS overrides the zfs/zpool exec client (default: real binaries;
	// tests inject a file-backed fake).
	ZFS *zfs.Exec

	// AttachNBD connects the volume's NBD socket to a /dev/nbd device
	// and publishes /dev/exvol/<id> (default: device.Attach shelling
	// out to nbd-client; tests fake it).
	AttachNBD func(sock, volID, nbdDev string) error

	// Tick is the reconcile cadence (default 2s; tests speed this up).
	Tick time.Duration

	// ListenAddr is the replication listener host pattern (default
	// ":%d" — all interfaces; tests bind per-node loopback IPs).
	ListenAddr string
}

// Runtime is one node's volume runtime.
type Runtime struct {
	opts Options
	zfs  *zfs.Exec
	lm   *lease.Manager

	mu        sync.Mutex
	srv       *transport.Server // shared per-node replication listener
	secs      map[string]*secondary.Secondary
	prim      map[string]*volPrimary
	nbdSlots  map[string]string       // volID → /dev/nbdN (assigned once)
	zvolOf    map[string]string       // volID → local zvol path
	specOf    map[string]storage.Spec // volID → spec (device reopen after receive)
	lastSnap  map[string]time.Time
	resyncing map[string]int // node ID → in-flight resyncs (cap 2 per node)

	convMu       sync.Mutex
	convInFlight bool // background replica-convergence run in flight
	recv         map[string]*resyncRecv
}

type volPrimary struct {
	coord      *primary.Coordinator
	dev        *device.ExvolDevice
	nsrv       *device.Server
	held       *lease.Held
	writer     *localwrite.Writer
	conns      map[string]*transport.Conn
	lastVerify time.Time
	verifySeq  uint64 // currency-watchdog rotation cursor (next op to re-check)
}

// New builds the runtime. Call Run in a goroutine.
func New(opts Options) *Runtime {
	if opts.Port == 0 {
		opts.Port = config.PortExvol
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = 10 * time.Second
	}
	if opts.SnapshotInterval <= 0 {
		opts.SnapshotInterval = 60 * time.Second
	}
	if opts.SnapshotKeep <= 0 {
		opts.SnapshotKeep = resync.DefaultKeep
	}
	if opts.ResyncBytesPerSec <= 0 {
		opts.ResyncBytesPerSec = 100 << 20
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.ZvolDevBase == "" {
		opts.ZvolDevBase = "/dev/zvol"
	}
	if opts.ZFS == nil {
		opts.ZFS = zfs.New()
	}
	if opts.AttachNBD == nil {
		opts.AttachNBD = func(sock, volID, nbdDev string) error {
			return device.Attach(sock, volID, nbdDev, device.ExecRunner{})
		}
	}
	return &Runtime{
		opts:      opts,
		zfs:       opts.ZFS,
		lm:        lease.NewManager(opts.St, opts.NodeID),
		secs:      map[string]*secondary.Secondary{},
		prim:      map[string]*volPrimary{},
		nbdSlots:  map[string]string{},
		zvolOf:    map[string]string{},
		specOf:    map[string]storage.Spec{},
		lastSnap:  map[string]time.Time{},
		resyncing: map[string]int{},
		recv:      map[string]*resyncRecv{},
	}
}

// Run reconciles every 2s until the context is done.
func (r *Runtime) Run(ctx context.Context) {
	tick := r.opts.Tick
	if tick <= 0 {
		tick = 2 * time.Second
	}
	t := time.NewTicker(tick)
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
		if status.State == storage.StateNeedsManualRecovery {
			// §9: an operator must choose a branch; the runtime never
			// auto-recovers a diverged volume (T15 `volume diverged`).
			r.maybeManualRecovery(ctx, spec.ID)
			continue
		}
		if status.State == storage.StateDeleting {
			// Deletion (T13 controller marks it): destroy the local
			// zvol, stop local roles; the controller drops the records.
			r.teardownVolume(ctx, id, status)
			continue
		}
		mine, zvolPath := r.myReplica(spec.ID, status)
		if mine {
			devNode, err := r.ensureZvol(ctx, spec, zvolPath)
			if err != nil {
				r.opts.Logger.Warn("zvol ensure failed", "vol", id, "err", err)
				continue
			}
			if status.Primary == r.opts.NodeID && status.State != storage.StateDeleting &&
				status.State != storage.StateNeedsManualRecovery {
				// §4.3 lease-loss demotion: a primary that lost the
				// volume lease (expired under partition, lost a CAS
				// race) must stop serving — the new holder may already
				// be recovering. Serving on would be split brain.
				r.demoteIfLeaseLost(ctx, spec.ID)
				if err := r.ensurePrimary(ctx, spec, status, zvolPath, devNode); err != nil {
					r.opts.Logger.Warn("primary ensure failed", "vol", id, "err", err)
				} else {
					r.reportSequence(ctx, spec.ID, status)
					r.verifyServingCurrent(ctx, spec.ID)
					r.periodicSnapshot(id, zvolPath)
					// Replica convergence (adopt + resync) dials peers
					// (up to ~12s of timeouts when a replica is down)
					// and streams full images — never inline in the
					// reconcile loop: a stalled dial must not freeze
					// demotion checks or every other volume's tick.
					r.convergeReplicas(ctx, spec, status, zvolPath)
				}
			}
			if status.Primary != r.opts.NodeID {
				// Role changed away from us (re-election after our
				// crash, operator move): stop the local write path —
				// still holding the (now invalid) lease would block
				// the new primary's acquisition for a full TTL.
				r.stopPrimaryIfServing(spec.ID)
				r.ensureSecondary(spec.ID, spec, zvolPath, devNode)
			}
		}
	}
	r.processResyncOps(ctx, seen)
	r.publishPoolStatus(ctx)
	r.pruneNotIn(seen)
	return nil
}

// opsOf lists the operation records under /volumes/_ops/<kind>/<volID>.
func opsOf(ctx context.Context, st store.Store, kind string) (map[string]*store.Entry, error) {
	res, err := st.List(ctx, store.Key("/volumes/_ops/"+kind+"/"))
	if err != nil {
		return nil, err
	}
	out := map[string]*store.Entry{}
	for _, e := range res {
		id := strings.TrimPrefix(strings.TrimPrefix(string(e.Key), "/volumes/_ops/"+kind+"/"), "/")
		out[id] = e
	}
	return out, nil
}

// delOp removes a consumed operation record.
func delOp(ctx context.Context, st store.Store, kind, volID string) {
	_ = st.Delete(ctx, store.Key("/volumes/_ops/"+kind+"/"+volID), 0)
}

// maybeManualRecovery consumes an operator's `diverged --choose`
// decision (§9's human-in-the-loop valve). The record
// /volumes/_ops/recover/<volID> holds {"choose": "<nodeID>"}; only the
// chosen node acts: it adopts ITS durable branch as the volume's truth
// (its secondary oplog's last seq), becomes primary, and lets T12
// resync bring the other replicas in line. All copies are preserved —
// the losing branch's data is never touched by this path.
func (r *Runtime) maybeManualRecovery(ctx context.Context, volID string) {
	ops, err := opsOf(ctx, r.opts.St, "recover")
	if err != nil {
		return
	}
	e, ok := ops[volID]
	if !ok {
		return
	}
	var req struct {
		Choose string `json:"choose"`
	}
	if json.Unmarshal(e.Value, &req) != nil || req.Choose == "" {
		r.opts.Logger.Warn("malformed recover op — ignoring", "vol", volID)
		return
	}
	if req.Choose != r.opts.NodeID {
		return // some other node is the chosen branch holder
	}
	r.mu.Lock()
	sec := r.secs[volID]
	r.mu.Unlock()
	if sec == nil {
		// The chosen node must hold a replica with an oplog; without
		// one we cannot know where its branch ends. The operator should
		// choose a node that holds a replica.
		r.opts.Logger.Error("diverged volume chosen here but no local replica/oplog; refusing", "vol", volID)
		return
	}
	lastSeq := sec.LastSeq()
	status, rev, err := storage.LoadStatus(ctx, r.opts.St, volID)
	if err != nil {
		return
	}
	status.State = storage.StateDegraded // healthy once resync converges
	status.Primary = r.opts.NodeID
	status.Sequence = lastSeq
	status.ManualRecovered = true
	if err := storage.CompareAndSwapStatus(ctx, r.opts.St, volID, rev, status); err != nil {
		r.opts.Logger.Warn("manual recovery CAS failed", "vol", volID, "err", err)
		return
	}
	delOp(ctx, r.opts.St, "recover", volID)
	r.opts.Logger.Info("operator chose this branch — adopting as truth",
		"vol", volID, "seq", lastSeq)
}

// resyncOp is a forced `volume resync` request (T15 §4.8).
type resyncOp struct {
	Replica string `json:"replica"`
	Full    bool   `json:"full"`
}

// processResyncOps handles forced resyncs for volumes this node is
// primary of. The op's full flag maps to T12's runResync (full = no
// common-ancestor increment, with the spec-required WARNING).
func (r *Runtime) processResyncOps(ctx context.Context, seen map[string]bool) {
	ops, err := opsOf(ctx, r.opts.St, "resync")
	if err != nil {
		return
	}
	for volID, e := range ops {
		if !seen[volID] {
			continue
		}
		var op resyncOp
		if json.Unmarshal(e.Value, &op) != nil || op.Replica == "" {
			delOp(ctx, r.opts.St, "resync", volID)
			continue
		}
		status, _, err := storage.LoadStatus(ctx, r.opts.St, volID)
		if err != nil || status.Primary != r.opts.NodeID {
			continue // not the primary — the primary's runtime consumes it
		}
		zp := r.zvolOf[volID]
		if zp == "" {
			continue // zvol not ensured yet; next tick
		}
		if r.resyncInFlight(op.Replica) >= 2 {
			continue // §4.6 cap
		}
		if err := r.runResync(ctx, volID, op.Replica, zp, status); err != nil {
			r.opts.Logger.Warn("forced resync failed", "vol", volID, "target", op.Replica, "err", err)
			continue // keep the op for retry
		}
		delOp(ctx, r.opts.St, "resync", volID)
	}
}

// lastPoolPub gates pool-status publication to once a minute.
var lastPoolPub struct {
	mu sync.Mutex
	at time.Time
}

// publishPoolStatus writes this node's zpool health into the cluster
// store (`expanse ctl storage pools` reads it; §4.8).
func (r *Runtime) publishPoolStatus(ctx context.Context) {
	lastPoolPub.mu.Lock()
	due := time.Since(lastPoolPub.at) >= time.Minute
	if due {
		lastPoolPub.at = time.Now()
	}
	lastPoolPub.mu.Unlock()
	if !due || r.opts.Pool == "" {
		return
	}
	ps, err := r.zfs.PoolStatus(ctx, r.opts.Pool)
	if err != nil {
		return // no pool yet (VM tests start before mkpool)
	}
	info, err := json.Marshal(map[string]any{
		"pool": r.opts.Pool, "health": ps.Health, "errors": ps.Errors,
	})
	if err != nil {
		return
	}
	_, _ = r.opts.St.Put(ctx, store.Key("/nodes/"+r.opts.NodeID+"/storage/pool"), info)
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
	dev := r.opts.ZvolDevBase + "/" + zvolPath
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
	r.zvolOf[volID] = zvolPath
	r.specOf[volID] = spec
	if sec, ok := r.secs[volID]; ok {
		r.mu.Unlock()
		if sec.HasWriter() {
			return
		}
		// Heal a quiesced secondary (writer nil since a resync receive
		// was abandoned mid-stream): reopen the device and swap it in,
		// keeping protocol + oplog state.
		if w, err := localwrite.Open(devNode, int64(spec.SizeBytes)); err == nil {
			if old := sec.SwapWriter(w); old != nil {
				if c, ok := old.(io.Closer); ok {
					_ = c.Close()
				}
			}
			r.opts.Logger.Info("secondary re-armed", "vol", volID, "zvol", zvolPath)
		} else {
			r.opts.Logger.Warn("secondary re-arm failed", "vol", volID, "err", err)
		}
		return
	}
	defer r.mu.Unlock()
	if err := r.ensureServerLocked(); err != nil {
		r.opts.Logger.Warn("replication server unavailable", "err", err)
		return
	}
	w, err := localwrite.Open(devNode, int64(spec.SizeBytes))
	if err != nil {
		r.opts.Logger.Warn("local writer open failed", "vol", volID, "err", err)
		return
	}
	sec := secondary.New(r.opts.NodeID, int(spec.SizeBytes), w)
	// Durable oplog (§4.3 4a): a daemon restart must not forget which
	// sequences the zvol holds — recovery would misjudge the replica
	// and FetchOps could not serve ops it no longer remembers (§4.3
	// failover deadlocks). The log lives INSIDE the pool's volumes
	// dataset so it shares the zvol's crash domain: whatever txg loss
	// erased zvol data erases the same records here, keeping the
	// metadata truthful. Records are fsynced on flush markers (the
	// client-visible durability barrier).
	mp, mpErr := r.zfs.Mountpoint(context.Background(), r.opts.Pool+"/volumes")
	if mpErr != nil || mp == "" {
		// A durable oplog is required for truthful recovery (§4.3 4a);
		// running memory-only must be loud.
		r.opts.Logger.Warn("durable oplog unavailable (memory-only replica)", "vol", volID, "err", mpErr)
	}
	if mpErr == nil && mp != "" {
		dir := mp + "/.oplogs"
		if err := os.MkdirAll(dir, 0o700); err == nil {
			if err := sec.SetOplogStore(fmt.Sprintf("%s/%s.oplog", dir, volID)); err != nil {
				r.opts.Logger.Warn("oplog store open failed (memory-only)", "vol", volID, "err", err)
			}
		}
	}
	r.secs[volID] = sec
	r.opts.Logger.Info("secondary serving", "vol", volID, "zvol", zvolPath)
}

func (r *Runtime) ensureServerLocked() error {
	if r.srv != nil {
		return nil
	}
	pat := r.opts.ListenAddr
	if pat == "" {
		pat = ":%d"
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(pat, r.opts.Port))
	if err != nil {
		return err
	}
	r.srv = transport.NewServer(ln, func(volID string) (transport.Handler, error) {
		// Runs on the server's connection goroutines: the secs map is
		// rewritten by stopAll — read under r.mu.
		r.mu.Lock()
		sec, ok := r.secs[volID]
		r.mu.Unlock()
		if !ok {
			return nil, experrors.New(experrors.KindNotFound, "exvol.runtime", "no such volume: "+volID)
		}
		return sec.Handler(), nil
	}, r.receiveChunk, 0)
	r.srv.SetQueryHandler(r.querySeq)
	r.srv.SetFetchHandler(r.fetchOps)
	r.srv.SetSnapListHandler(r.listSnaps)
	r.srv.SetAdoptSeqHandler(r.adoptSeq)
	go r.srv.Serve() //nolint:errcheck — Close during shutdown is fine
	return nil
}

// --- recovery / resync handlers served by THIS node's transport server ---

// querySeq answers a recovery probe (§4.3 step 4a) from the local
// secondary's op log.
func (r *Runtime) querySeq(volID string) (*pb.SeqQueryReply, error) {
	r.mu.Lock()
	sec := r.secs[volID]
	r.mu.Unlock()
	if sec == nil {
		return nil, experrors.New(experrors.KindNotFound, "exvol.runtime.querySeq", "no such volume: "+volID)
	}
	rep := &pb.SeqQueryReply{VolId: volID, LastSeq: sec.LastSeq()}
	for seq, rec := range sec.OpLog() {
		rep.Ops = append(rep.Ops, &pb.SeqInfo{Seq: seq, Crc32C: rec.CRC})
	}
	return rep, nil
}

// fetchOps serves recovery op pulls (§4.3 steps 4b/4c) by re-reading
// this replica's durable copy.
func (r *Runtime) fetchOps(volID string, from, to uint64) (*pb.FetchOpsReply, error) {
	r.mu.Lock()
	sec := r.secs[volID]
	r.mu.Unlock()
	if sec == nil {
		return nil, experrors.New(experrors.KindNotFound, "exvol.runtime.fetchOps", "no such volume: "+volID)
	}
	ops, err := sec.FetchOps(from, to)
	if err != nil {
		return nil, err
	}
	out := &pb.FetchOpsReply{}
	for _, op := range ops {
		out.Ops = append(out.Ops, &pb.WriteRequest{
			VolId: volID, Seq: op.Seq, Offset: op.Offset,
			Data: op.Data, Crc32C: op.CRC, Flush: op.Flush,
		})
	}
	return out, nil
}

// listSnaps answers resync snapshot comparison for the local zvol.
func (r *Runtime) listSnaps(volID string) (*pb.SnapListReply, error) {
	r.mu.Lock()
	zp := r.zvolOf[volID]
	r.mu.Unlock()
	if zp == "" {
		return nil, experrors.New(experrors.KindNotFound, "exvol.runtime.listSnaps", "no such volume: "+volID)
	}
	snaps, err := r.zfs.ListSnapshots(context.Background(), zp)
	if err != nil {
		return nil, err
	}
	return &pb.SnapListReply{Names: snaps}, nil
}

// adoptSeq latches a replica's post-resync sequence (§4.3 resync
// step 5).
func (r *Runtime) adoptSeq(volID string, seq uint64, full bool) error {
	r.mu.Lock()
	sec := r.secs[volID]
	r.mu.Unlock()
	if sec == nil {
		return experrors.New(experrors.KindNotFound, "exvol.runtime.adoptSeq", "no such volume: "+volID)
	}
	sec.AdoptResync(seq, full)
	return nil
}

// resyncRecv is one in-progress `zfs receive` for a volume (the remote
// end of a resync stream).
type resyncRecv struct {
	pw     *io.PipeWriter // chunks are written here
	count  int64
	done   chan error
	stderr *bytes.Buffer // zfs receive diagnostics (logged on failure)
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
	// Dial every other replica; the recovery probe and the write
	// fan-out share the connections. An unreachable replica does NOT
	// abort bring-up (§4.3 4d): recovery marks it Stale and serving
	// proceeds with the reachable quorum; adoptNewReplicas re-dials
	// and re-admits it when it returns.
	conns := map[string]*transport.Conn{}
	for _, p := range status.Placement {
		if p.NodeID == r.opts.NodeID {
			continue
		}
		conn, derr := r.dialReplica(ctx, p.NodeID)
		if derr != nil {
			r.opts.Logger.Warn("replica unreachable; proceeding without it (§4.3 4d)",
				"vol", spec.ID, "node", p.NodeID, "err", derr)
			continue
		}
		conns[p.NodeID] = conn
	}
	if len(conns) == 0 && status.Sequence > 0 {
		// No replica reachable AND the store claims prior writes: the
		// resume sequence cannot be verified — do not serve (retrying
		// next tick risks neither ack-loss nor split-brain).
		w.Close()
		r.lm.Release(ctx, held)
		return fmt.Errorf("no replica reachable; resume seq %d unverifiable", status.Sequence)
	}

	// §4.3 recovery: the failover algorithm (4a probes, 4b pulls, 4c
	// leveling, 4d stale marks) ALWAYS runs before serving. The
	// store's status.Sequence is written asynchronously and can lag
	// behind acked writes (or be lost entirely with a hard-killed
	// primary), so it is NOT a trusted resume point — the replicas'
	// probed op logs are. A fresh volume probes back 0 everywhere and
	// resumes at 0.
	startSeq := status.Sequence
	var recRes recovery.Result
	if status.ManualRecovered {
		// §9 via T15: the operator chose this branch; adopt the
		// recorded sequence without re-running automatic recovery
		// (it would re-detect the divergence we were told to accept).
		r.opts.Logger.Info("manual recovery: adopting branch as primary", "vol", spec.ID, "seq", startSeq)
	} else {
		recRes, startSeq, err = r.recoverVol(ctx, spec.ID, w, conns)
		if err != nil {
			w.Close()
			r.lm.Release(ctx, held)
			for _, cn := range conns {
				cn.Close()
			}
			var div *recovery.DivergedError
			var unfill *recovery.UnfillableError
			if errors.As(err, &div) {
				// §9: refuse automatic recovery, preserve all copies,
				// hand the decision to an operator.
				r.markDiverged(ctx, spec.ID)
				return err
			}
			if errors.As(err, &unfill) {
				// An op some replica CLAIMS but nobody can serve
				// honestly is suspected data loss — retrying is the
				// livelock the VM runs demonstrated. Preserve copies,
				// stop the automatic loop, involve an operator.
				r.opts.Logger.Error("recovery: unfillable op — claimed durability is missing; manual recovery required", "vol", spec.ID, "seq", unfill.Seq, "err", unfill.Err)
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
	// Recovery's stale marks (unreachable replicas §4.3 4d, and 4c
	// targets that could not be leveled) MUST outlive recovery: without
	// them the coordinator counts a replica whose data never arrived as
	// quorum — the resync that repairs it never triggers, and the hole
	// persists forever. A node still crashed (not in reps) is picked up
	// by the adopt path (adopt → MarkStale → resync) when it returns.
	for _, staleID := range recRes.Stale {
		coord.MarkStale(staleID)
	}
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
	if err := r.opts.AttachNBD(sock, spec.ID, r.nbdDevFor(spec.ID)); err != nil {
		nsrv.Close()
		w.Close()
		r.lm.Release(ctx, held) // free the lease; the next tick retries
		return err
	}

	r.mu.Lock()
	r.prim[spec.ID] = &volPrimary{coord: coord, dev: dev, nsrv: nsrv, held: held, writer: w, conns: conns}
	r.mu.Unlock()
	r.clearManualRecovered(ctx, spec.ID)
	r.opts.Logger.Info("primary serving", "vol", spec.ID, "zvol", zvolPath, "replicas", len(reps), "seq", startSeq)
	return nil
}

// WriteOp issues one replicated data write through the volume's
// primary coordinator (§4.3 quorum path). The NBD device is the
// production client path; this is the programmatic ops surface.
func (r *Runtime) WriteOp(volID string, off int64, data []byte) error {
	r.mu.Lock()
	p := r.prim[volID]
	r.mu.Unlock()
	if p == nil {
		return experrors.New(experrors.KindUnavailable, "exvol.runtime.WriteOp", "not primary for "+volID)
	}
	return p.coord.Write(data, off)
}

// FlushOp issues a replicated flush marker (the client-visible
// durability barrier — data + oplog are fsynced at quorum).
func (r *Runtime) FlushOp(volID string) error {
	r.mu.Lock()
	p := r.prim[volID]
	r.mu.Unlock()
	if p == nil {
		return experrors.New(experrors.KindUnavailable, "exvol.runtime.FlushOp", "not primary for "+volID)
	}
	return p.coord.Flush()
}

// clearManualRecovered drops the one-shot §9 operator-choice flag once
// the primary is up (a later divergence must re-trigger manual mode).
func (r *Runtime) clearManualRecovered(ctx context.Context, volID string) {
	status, rev, err := storage.LoadStatus(ctx, r.opts.St, volID)
	if err != nil || !status.ManualRecovered {
		return
	}
	status.ManualRecovered = false
	if err := storage.CompareAndSwapStatus(ctx, r.opts.St, volID, rev, status); err != nil {
		r.opts.Logger.Warn("clear manual_recovered failed", "vol", volID, "err", err)
	}
}

// recoverVol runs the §4.3 failover recovery algorithm (4a–4d) against
// the other replicas over their live transport connections, returning
// the seq the new primary resumes at (step 5). It returns a
// *recovery.DivergedError when the branches have diverged (§9) — the
// caller must NOT serve and must preserve all copies.
func (r *Runtime) recoverVol(ctx context.Context, volID string, w *localwrite.Writer, conns map[string]*transport.Conn) (recovery.Result, uint64, error) {
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
	// The CALLING node's own durable copy may be behind the survivors:
	// it acked ops under the old primary, but a restart (or the §4.3
	// re-election itself) can leave its zvol missing ops the probes
	// report. Recovery 4b only fills the ELECTED node — the controller
	// may have elected THIS node. Before serving, pull every op the
	// local copy lacks from a reachable holder (mirror of 4b).
	localLast := r.localLastSeq(volID)
	// selfFetch re-reads ops from THIS node's durable copy for 4c
	// leveling: claims from the local oplog, bytes via the writer.
	selfFetch := func(_ context.Context, from, to uint64) ([]protocol.WriteOp, error) {
		var log map[uint64]secondary.OpRecord
		if sec := r.secOf(volID); sec != nil {
			log = sec.OpLog()
		} else {
			log = map[uint64]secondary.OpRecord{}
		}
		ops := make([]protocol.WriteOp, 0, to-from)
		for seq := from + 1; seq <= to; seq++ {
			rec, ok := log[seq]
			if !ok {
				return nil, experrors.New(experrors.KindUnavailable, "exvol.recoverVol.selfFetch",
					fmt.Sprintf("op %d not in local oplog (filled by pull, not logged) — target must resync", seq))
			}
			op := protocol.WriteOp{Seq: seq, Offset: uint64(rec.Offset), CRC: rec.CRC}
			if rec.Length > 0 {
				buf := make([]byte, rec.Length)
				if _, err := w.ReadAt(buf, int64(rec.Offset)); err != nil {
					return nil, experrors.Wrap(err, experrors.KindInternal, "exvol.recoverVol.selfFetch", "self re-read")
				}
				op.Data = buf
			} else {
				op.Flush = true
			}
			ops = append(ops, op)
		}
		return ops, nil
	}
	res, err := recovery.Recover(ctx, probes, selfFetch,
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
		return recovery.Result{}, 0, err
	}
	r.opts.Logger.Info("volume recovered", "vol", volID,
		"primary", res.NewPrimaryID, "seq", res.MaxSeq, "pulled", res.Pulled, "stale", res.Stale)
	// Fill the local copy (see above) before the caller resumes serving.
	if res.MaxSeq > localLast {
		byID := map[string]*recovery.Probe{}
		for i := range probes {
			byID[probes[i].NodeID] = &probes[i]
		}
		for seq := localLast + 1; seq <= res.MaxSeq; seq++ {
			// Honest-holder rule (mirror of 4b): try claimants in turn,
			// CRC-verifying bytes against each holder's own claim; a
			// holder that fails or fabricates is skipped, not fatal —
			// but if NOBODY can serve an op the cluster claims is
			// durable, that is data loss: refuse to serve (the
			// fabricated-zeros bug filled the local copy with garbage
			// and called it recovery).
			op, err := recovery.FillOne(ctx, probes, seq)
			if err != nil {
				return res, res.MaxSeq, &recovery.UnfillableError{Seq: seq, Err: err}
			}
			if op.Flush {
				err = w.Flush()
			} else {
				err = w.WriteAt(op.Data, int64(op.Offset))
			}
			if err != nil {
				return res, res.MaxSeq, fmt.Errorf("local fill: apply op %d: %w", seq, err)
			}
		}
		r.opts.Logger.Info("local copy filled from survivors", "vol", volID,
			"from", localLast, "to", res.MaxSeq)
	}
	return res, res.MaxSeq, nil
}

// verifyServingInterval bounds how often the currency watchdog samples
// the serving primary's own zvol (cheap: one oplog fetch + one re-read).
const verifyServingInterval = 1 * time.Second

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

func crc32c(b []byte) uint32 { return crc32.Checksum(b, crc32cTable) }

// verifyServingCurrent is the §4.3 promotion-safety watchdog (T17.3):
// a serving primary re-checks per tick that its local copy is still
// current. Storage can lose or tear bytes while reads keep "succeeding"
// (a truncated zvol reads back zeros), so every verify tick the
// watchdog re-reads ONE acked op's bytes — rotating through the whole
// history via the replica's per-op CRC claims — and compares them
// against the replica's durable record. Only a positive mismatch is
// evidence (a flaky fetch is not); the response is to step down:
// re-promotion re-runs recovery, which re-pulls the missing ops from
// the replica and heals.
func (r *Runtime) verifyServingCurrent(ctx context.Context, volID string) {
	r.mu.Lock()
	p := r.prim[volID]
	if p == nil || p.coord == nil || p.writer == nil {
		r.mu.Unlock()
		return
	}
	if time.Since(p.lastVerify) < verifyServingInterval {
		r.mu.Unlock()
		return
	}
	p.lastVerify = time.Now()
	cursor := p.verifySeq
	r.mu.Unlock()

	last := p.coord.LastSeq()
	if last == 0 {
		return // nothing acked yet; a fresh volume is current by definition
	}
	var conn string
	for _, id := range p.coord.ReplicaIDs() {
		if c := p.conns[id]; c != nil {
			conn = id
			break
		}
	}
	if conn == "" {
		return // no live replica to compare against; 4d handles isolation
	}
	// NOTE: the pump and the recovery paths own the shared conns; a
	// transport Conn is single-submitter, so the watchdog dials its own
	// short-lived connection per verify instead of reusing p.conns.
	probe, err := r.dialReplica(ctx, conn)
	if err != nil {
		return // availability first: a failed dial is not divergence
	}
	defer probe.Close()
	// Pick the next claimed op after the cursor (wrapping): the rotation
	// walks the ENTIRE acked history over time, not just the hot tail.
	qrep, err := probe.QuerySeq(volID)
	if err != nil {
		return // availability first: a failed probe is not divergence
	}
	claims := map[uint64]struct{}{}
	var minSeq uint64
	for _, op := range qrep.GetOps() {
		claims[op.GetSeq()] = struct{}{}
		if minSeq == 0 || op.GetSeq() < minSeq {
			minSeq = op.GetSeq()
		}
	}
	seq, ok := cursor+1, false
	for seq <= last {
		if _, claimed := claims[seq]; claimed {
			ok = true
			break
		}
		seq++
	}
	if !ok {
		if minSeq == 0 || minSeq > last {
			return // replica's log covers nothing comparable
		}
		seq, ok = minSeq, true // wrap to the oldest claimed op
	}
	p.verifySeq = seq

	frep, err := probe.FetchOps(volID, seq-1, seq)
	if err != nil || len(frep.GetOps()) == 0 {
		return // availability first: a failed fetch is not divergence
	}
	op := frep.GetOps()[0]
	if op.GetFlush() || len(op.GetData()) == 0 {
		return // flush markers carry no bytes to compare
	}
	buf := make([]byte, len(op.GetData()))
	if _, err := p.writer.ReadAt(buf, int64(op.GetOffset())); err != nil {
		r.opts.Logger.Error("currency watchdog: primary cannot re-read its own zvol — stepping down",
			"vol", volID, "seq", seq, "err", err)
		r.stopPrimary(volID)
		return
	}
	if crc32c(buf) != crc32c(op.GetData()) {
		r.opts.Logger.Error("currency watchdog: primary zvol diverges from the replica's durable record — stepping down for re-recovery",
			"vol", volID, "seq", seq, "off", op.GetOffset())
		r.stopPrimary(volID)
	}
}

// demoteIfLeaseLost stops the local primary for volID when its volume
// lease is no longer valid. Idempotent; a no-op when not serving.
func (r *Runtime) demoteIfLeaseLost(ctx context.Context, volID string) {
	r.mu.Lock()
	p, ok := r.prim[volID]
	r.mu.Unlock()
	if !ok || p.held.Valid() {
		return
	}
	r.opts.Logger.Warn("volume lease lost — demoting primary (§4.3)", "vol", volID)
	r.stopPrimary(volID)
}

// stopPrimaryIfServing stops the local primary when the store no
// longer names this node as the volume's primary (idempotent).
func (r *Runtime) stopPrimaryIfServing(volID string) {
	r.mu.Lock()
	_, serving := r.prim[volID]
	r.mu.Unlock()
	if serving {
		r.opts.Logger.Warn("demoting primary: no longer the elected primary (§4.3)", "vol", volID)
		r.stopPrimary(volID)
	}
}

// stopPrimary tears down the local write path for volID (lease lost,
// role change); the next reconcile tick re-evaluates the role.
func (r *Runtime) stopPrimary(volID string) {
	r.mu.Lock()
	p, ok := r.prim[volID]
	if ok {
		delete(r.prim, volID)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	p.nsrv.Close()
	p.held.Abandon()
	p.writer.Close()
	for _, cn := range p.conns {
		cn.Close()
	}
}

// localLastSeq is the calling node's own durable-copy last sequence for
// volID (its secondary's oplog view), 0 when it holds no replica.
func (r *Runtime) localLastSeq(volID string) uint64 {
	sec := r.secOf(volID)
	if sec == nil {
		return 0
	}
	return sec.LastSeq()
}

// secOf returns the local secondary for volID, if this node holds one.
func (r *Runtime) secOf(volID string) *secondary.Secondary {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.secs[volID]
}

// markDiverged records §9's NeedsManualRecovery state — automatic
// recovery is refused and every copy is left exactly as found.
func (r *Runtime) markDiverged(ctx context.Context, volID string) {
	// The status record is contended (sequence reporters, the
	// controller); a losing CAS here would silently leave the volume
	// Healthy and the automatic retry loop alive — retry until it
	// lands, and say so when it does not.
	var lastErr error
	for i := 0; i < 20; i++ {
		status, rev, err := storage.LoadStatus(ctx, r.opts.St, volID)
		if err != nil {
			lastErr = err
		} else if status.State != storage.StateNeedsManualRecovery {
			status.State = storage.StateNeedsManualRecovery
			lastErr = storage.CompareAndSwapStatus(ctx, r.opts.St, volID, rev, status)
		} else {
			lastErr = nil
		}
		if lastErr == nil {
			r.opts.Logger.Error("volume diverged — manual recovery required, all copies preserved", "vol", volID)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	r.opts.Logger.Error("volume diverged — FAILED to record NeedsManualRecovery", "vol", volID, "err", lastErr)
}

// dialReplica connects to a node's replication transport with a short
// retry budget. Callers treat failure as "replica unreachable" (§4.3
// 4d: marked Stale, re-adopted later by adoptNewReplicas) — a long
// retry here would stall failover recovery on a hard-killed node.
func (r *Runtime) dialReplica(ctx context.Context, nodeID string) (*transport.Conn, error) {
	addr, err := r.opts.AddrOf(nodeID)
	if err != nil {
		return nil, err
	}
	target := fmt.Sprintf("%s:%d", addr, r.opts.Port)
	var last error
	for i := 0; i < 3; i++ {
		// Per-attempt deadline: a cold WireGuard peer (or a filtered
		// route) drops SYNs silently, and an unbounded connect would
		// hang the reconcile loop for the kernel's ~2min timeout.
		dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		cn, derr := transport.Dial(dctx, target)
		cancel()
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

// teardownVolume destroys the local zvol and all local roles for a
// deleting volume (idempotent — retried ticks no-op).
func (r *Runtime) teardownVolume(ctx context.Context, volID string, status storage.Status) {
	r.mu.Lock()
	if p, ok := r.prim[volID]; ok {
		p.nsrv.Close()
		p.held.Abandon()
		p.writer.Close()
		for _, cn := range p.conns {
			cn.Close()
		}
		delete(r.prim, volID)
	}
	delete(r.secs, volID)
	zp := r.zvolOf[volID]
	if zp == "" {
		for _, pl := range status.Placement {
			if pl.NodeID == r.opts.NodeID && pl.ZvolPath != "" {
				zp = pl.ZvolPath
				break
			}
		}
	}
	r.mu.Unlock()
	if zp != "" {
		if err := r.zfs.DestroyZvol(ctx, zp, true); err != nil {
			r.opts.Logger.Warn("zvol destroy failed", "vol", volID, "err", err)
		} else {
			r.mu.Lock()
			delete(r.zvolOf, volID)
			r.mu.Unlock()
			r.opts.Logger.Info("zvol destroyed", "vol", volID, "zvol", zp)
		}
	}
}

// reportSequence publishes the primary's last assigned seq into its
// placement row (the T13 controller's election input).
func (r *Runtime) reportSequence(ctx context.Context, volID string, status storage.Status) {
	r.mu.Lock()
	p := r.prim[volID]
	r.mu.Unlock()
	if p == nil || p.coord == nil {
		return
	}
	seq := p.coord.LastSeq()
	for i := range status.Placement {
		if status.Placement[i].NodeID == r.opts.NodeID && status.Placement[i].Sequence != seq {
			status.Placement[i].Sequence = seq
			if st, rev, err := storage.LoadStatus(ctx, r.opts.St, volID); err == nil {
				for j := range st.Placement {
					if st.Placement[j].NodeID == r.opts.NodeID {
						st.Placement[j].Sequence = seq
					}
				}
				_ = storage.CompareAndSwapStatus(ctx, r.opts.St, volID, rev, st)
			}
			return
		}
	}
}

// convergeReplicas runs adoptNewReplicas + resyncStale in the
// background, one run at a time per runtime.
func (r *Runtime) convergeReplicas(ctx context.Context, spec storage.Spec, status storage.Status, zvolPath string) {
	r.convMu.Lock()
	if r.convInFlight {
		r.convMu.Unlock()
		return
	}
	r.convInFlight = true
	r.convMu.Unlock()
	go func() {
		defer func() {
			r.convMu.Lock()
			r.convInFlight = false
			r.convMu.Unlock()
		}()
		r.adoptNewReplicas(ctx, spec, status, zvolPath)
		r.resyncStale(ctx, spec, status, zvolPath)
	}()
}

// adoptNewReplicas converges replica rebuilds (T13): a placement node
// not yet in the coordinator's fan-out is dialed and admitted — it
// starts behind, gets marked Stale on its first gap NACK, and T12's
// resync fills it.
func (r *Runtime) adoptNewReplicas(ctx context.Context, spec storage.Spec, status storage.Status, zvolPath string) {
	r.mu.Lock()
	p := r.prim[spec.ID]
	r.mu.Unlock()
	if p == nil || p.coord == nil {
		return
	}
	have := map[string]bool{}
	for _, id := range p.coord.ReplicaIDs() {
		have[id] = true
	}
	for _, pl := range status.Placement {
		if pl.NodeID == r.opts.NodeID || have[pl.NodeID] {
			continue
		}
		conn, err := r.dialReplica(ctx, pl.NodeID)
		if err != nil {
			r.opts.Logger.Warn("rebuild dial failed", "vol", spec.ID, "node", pl.NodeID, "err", err)
			continue
		}
		p.coord.AddReplica(pl.NodeID, transport.NewSender(conn, 0, 0))
		// §4.3 4d/5: the adopted replica's durable copy is unverified
		// (a node that lost its zvol rejoins EMPTY). Exclude it from
		// quorum and backfill from the latest snapshot before it may
		// ack writes again.
		p.coord.MarkStale(pl.NodeID)
		r.opts.Logger.Info("rebuild replica admitted (resyncing)", "vol", spec.ID, "node", pl.NodeID)
		if r.resyncInFlight(pl.NodeID) < 2 {
			if rerr := r.runResync(ctx, spec.ID, pl.NodeID, zvolPath, status); rerr != nil {
				r.opts.Logger.Warn("adopt resync failed", "vol", spec.ID, "node", pl.NodeID, "err", rerr)
			}
		}
	}
}

// pruneNotIn stops local roles for volumes that disappeared from the
// store (delete handling stays minimal in T10; T13 grows the rest).
func (r *Runtime) pruneNotIn(seen map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.secs {
		if !seen[id] {
			delete(r.secs, id)
			delete(r.zvolOf, id)
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

// receiveChunk is the transport rawHandler: the remote end of a resync
// stream. Payload = 2-byte BE volID length + volID + stream bytes; a
// zero-length data section closes the stream. The ack carries the
// cumulative bytes received (8 bytes BE).
func (r *Runtime) receiveChunk(payload []byte) ([]byte, error) {
	if len(payload) < 2 {
		return nil, fmt.Errorf("malformed resync chunk")
	}
	n := int(binary.BigEndian.Uint16(payload))
	if len(payload) < 2+n {
		return nil, fmt.Errorf("malformed resync chunk")
	}
	volID := string(payload[2 : 2+n])
	data := payload[2+n:]

	r.mu.Lock()
	rp := r.recv[volID]
	if rp == nil {
		zp := r.zvolOf[volID]
		if zp == "" {
			r.mu.Unlock()
			return nil, experrors.New(experrors.KindNotFound, "exvol.runtime.receiveChunk", "no such volume: "+volID)
		}
		r.mu.Unlock()
		// Quiesce the local device BEFORE spawning `zfs receive`: a
		// full-stream receive REPLACES the zvol dataset object (fresh
		// creation, prior snapshots destroyed). Holding our own
		// secondary's device fd open across the spawn pins the old
		// device — the /dev/zvol symlink and the block-device page
		// cache can then keep serving pre-receive content for the
		// lifetime of the fd, and the receive itself races the open fd
		// on the device it is about to replace. Re-armed by
		// reopenLocalDevice at stream end (success or failure).
		r.quiesceLocalDevice(volID)
		var stderr bytes.Buffer
		cmd := exec.Command(r.zfs.ZfsPath, "receive", "-F", zp)
		cmd.Stderr = &stderr
		rr, rw := io.Pipe()
		cmd.Stdin = rr
		done := make(chan error, 1)
		go func() { done <- cmd.Run() }()
		rp = &resyncRecv{pw: rw, done: done, stderr: &stderr}
		r.mu.Lock()
		r.recv[volID] = rp
	}
	rp.count += int64(len(data))
	count := rp.count
	if len(data) == 0 {
		delete(r.recv, volID)
	}
	r.mu.Unlock()

	if len(data) > 0 {
		if _, err := rp.pw.Write(data); err != nil {
			rp.pw.Close()
			return nil, err
		}
	} else {
		rp.pw.Close() // EOF: zfs receive finalizes the image
		err := <-rp.done
		r.reopenLocalDevice(volID)
		if err != nil {
			if rp.stderr != nil && rp.stderr.Len() > 0 {
				r.opts.Logger.Warn("zfs receive failed", "vol", volID, "stderr", rp.stderr.String())
			}
			return nil, fmt.Errorf("zfs receive: %w", err)
		}
	}
	ack := make([]byte, 8)
	binary.BigEndian.PutUint64(ack, uint64(count))
	return ack, nil
}

// quiesceLocalDevice closes this node's local writer for volID around
// a `zfs receive -F` (see receiveChunk). The secondary keeps its
// protocol + oplog state; it is re-armed by reopenLocalDevice, or by
// the reconcile tick's ensureSecondary heal path if the stream is
// abandoned.
func (r *Runtime) quiesceLocalDevice(volID string) {
	r.mu.Lock()
	sec := r.secs[volID]
	r.mu.Unlock()
	if sec == nil {
		return
	}
	if old := sec.SwapWriter(nil); old != nil {
		if c, ok := old.(io.Closer); ok {
			_ = c.Close()
		}
	}
}

// reopenLocalDevice re-opens the volume's device node and re-arms the
// secondary after a resync receive (quiesceLocalDevice's counterpart,
// run on BOTH receive success and failure — `receive -F` may have
// destroyed the dataset before failing). The device node may take a
// moment to reappear (udev re-creates the link for the new dataset
// object), so poll briefly before giving up.
func (r *Runtime) reopenLocalDevice(volID string) {
	r.mu.Lock()
	sec := r.secs[volID]
	zp := r.zvolOf[volID]
	spec, haveSpec := r.specOf[volID]
	r.mu.Unlock()
	if sec == nil || zp == "" || !haveSpec {
		return
	}
	devNode := r.opts.ZvolDevBase + "/" + zp
	deadline := time.Now().Add(10 * time.Second)
	for {
		w, err := localwrite.Open(devNode, int64(spec.SizeBytes))
		if err == nil {
			if old := sec.SwapWriter(w); old != nil {
				if c, ok := old.(io.Closer); ok {
					_ = c.Close()
				}
			}
			r.opts.Logger.Info("local device reopened after resync receive", "vol", volID, "dev", devNode)
			return
		}
		if time.Now().After(deadline) {
			// Loud, and self-healing: drop the secondary so the
			// reconcile tick rebuilds it — never serve FetchOps/querySeq
			// from a dead fd.
			r.opts.Logger.Warn("local device reopen failed after receive; dropping secondary for rebuild", "vol", volID, "err", err)
			r.mu.Lock()
			delete(r.secs, volID)
			r.mu.Unlock()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// periodicSnapshot takes the volume's @resync-<seq> snapshot on the
// configured cadence (§4.3, G6.6) and enforces retention.
func (r *Runtime) periodicSnapshot(volID, zvolPath string) {
	r.mu.Lock()
	p := r.prim[volID]
	if p == nil || p.coord == nil {
		r.mu.Unlock()
		return
	}
	if time.Since(r.lastSnap[volID]) < r.opts.SnapshotInterval {
		r.mu.Unlock()
		return
	}
	r.lastSnap[volID] = time.Now()
	seq := p.coord.LastSeq()
	r.mu.Unlock()
	if seq == 0 {
		return // nothing written yet
	}
	if _, err := resync.EnsureSnapshot(context.Background(), r.zfs, zvolPath, seq, r.opts.SnapshotKeep); err != nil {
		r.opts.Logger.Warn("resync snapshot failed", "vol", volID, "err", err)
	}
}

// resyncStale runs one resync per Stale replica (§4.3 resync), bounded
// to 2 concurrent resyncs per target node.
func (r *Runtime) resyncStale(ctx context.Context, spec storage.Spec, status storage.Status, zvolPath string) {
	r.mu.Lock()
	p := r.prim[spec.ID]
	if p == nil || p.coord == nil {
		r.mu.Unlock()
		return
	}
	stale := p.coord.StaleReplicas()
	r.mu.Unlock()
	for _, target := range stale {
		if r.resyncInFlight(target) >= 2 {
			continue // §4.6: max 2 resyncs per node
		}
		if err := r.runResync(ctx, spec.ID, target, zvolPath, status); err != nil {
			r.opts.Logger.Warn("resync failed", "vol", spec.ID, "target", target, "err", err)
			continue
		}
	}
}

func (r *Runtime) resyncInFlight(nodeID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resyncing[nodeID]
}

// runResync streams the primary's latest @resync-<seq> snapshot into a
// stale replica incrementally (common-ancestor send -i) or fully (with
// the spec-required WARNING), then re-admits it to quorum.
func (r *Runtime) runResync(ctx context.Context, volID, target, zvolPath string, status storage.Status) error {
	r.mu.Lock()
	p := r.prim[volID]
	if p == nil || p.coord == nil {
		r.mu.Unlock()
		return fmt.Errorf("no primary for %s", volID)
	}
	r.resyncing[target]++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.resyncing[target]--
		r.mu.Unlock()
	}()

	seq := p.coord.LastSeq()
	// seq == 0 (fresh volume, nothing written): there is no @resync-0 to
	// reference — take one anyway and full-send the (empty) zvol. The
	// old behavior ("has no writes to resync") retried forever, keeping
	// the replica stale indefinitely on an idle volume (the observed
	// livelock).
	if _, err := resync.EnsureSnapshot(ctx, r.zfs, zvolPath, seq, r.opts.SnapshotKeep); err != nil {
		return fmt.Errorf("resync snapshot: %w", err)
	}
	snaps, err := r.zfs.ListSnapshots(ctx, zvolPath)
	if err != nil {
		return fmt.Errorf("list source snapshots: %w", err)
	}

	// The resync connection: throttled (§4.3 bandwidth control) and
	// DSCP-marked CS1 so bulk traffic never starves foreground I/O.
	addr, err := r.opts.AddrOf(target)
	if err != nil {
		return err
	}
	targetAddr := fmt.Sprintf("%s:%d", addr, r.opts.Port)
	var dopts []transport.DialOption
	dopts = append(dopts, transport.WithResyncThrottle(transport.NewThrottle(r.opts.ResyncBytesPerSec)))
	dopts = append(dopts, transport.WithDSCP(transport.DSCPBulk))
	conn, err := transport.Dial(ctx, targetAddr, dopts...)
	if err != nil {
		return fmt.Errorf("dial resync target %s: %w", target, err)
	}
	defer conn.Close()

	sink := &remoteSink{conn: conn, volID: volID}
	out, err := resync.Run(ctx, r.zfs, zvolPath, snaps, sink, r.opts.Logger)
	if err != nil {
		return err
	}

	// Re-admit the replica with a fresh FOREGROUND connection (the
	// resync connection was throttled/DSCP-marked — never write I/O).
	fg, err := r.dialReplica(ctx, target)
	if err != nil {
		return fmt.Errorf("re-dial resynced replica: %w", err)
	}
	p.coord.AddReplica(target, transport.NewSender(fg, 0, 0))

	// Status: the replica is Secondary again, durable at the adopted seq.
	for i := range status.Placement {
		pl := &status.Placement[i]
		if pl.NodeID == target {
			pl.Role = storage.RoleSecondary
			pl.Healthy = true
			pl.Sequence = out.Adopt
			pl.LastSeen = time.Now()
		}
	}
	if st, rev, err := storage.LoadStatus(ctx, r.opts.St, volID); err == nil {
		st.Placement = status.Placement
		_ = storage.CompareAndSwapStatus(ctx, r.opts.St, volID, rev, st)
	}
	r.opts.Logger.Info("resync complete", "vol", volID, "target", target,
		"seq", out.Adopt, "incremental", !out.Full, "bytes", out.Bytes)
	return nil
}

// remoteSink drives a replica's receive side over the transport.
type remoteSink struct {
	conn  *transport.Conn
	volID string
}

func (s *remoteSink) ListSnaps(ctx context.Context) ([]string, error) {
	rep, err := s.conn.ListSnaps(s.volID)
	if err != nil {
		return nil, err
	}
	return rep.GetNames(), nil
}

// Receive streams r into the replica in framed chunks; each chunk is
// rate-limited by the connection throttle (§4.3 bandwidth control).
func (s *remoteSink) Receive(_ context.Context, src io.Reader) (int64, error) {
	buf := make([]byte, 256<<10) // 256 KiB chunks
	total := int64(0)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			total += int64(n)
			if _, err := s.conn.SendResyncChunk(mkChunk(s.volID, buf[:n])); err != nil {
				return total, err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return total, rerr
		}
	}
	// Zero-length chunk = end of stream (the replica finalizes
	// `zfs receive` and acks with its cumulative count).
	if _, err := s.conn.SendResyncChunk(mkChunk(s.volID, nil)); err != nil {
		return total, err
	}
	return total, nil
}

func mkChunk(volID string, data []byte) []byte {
	out := make([]byte, 2+len(volID)+len(data))
	binary.BigEndian.PutUint16(out, uint16(len(volID)))
	copy(out[2:], volID)
	copy(out[2+len(volID):], data)
	return out
}

func (s *remoteSink) AdoptSeq(ctx context.Context, seq uint64, full bool) error {
	return s.conn.AdoptSeq(s.volID, seq, full)
}
