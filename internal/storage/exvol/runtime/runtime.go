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
	"github.com/expanse/expanse/internal/storage/exvol/oplog"
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

	// TransientRecoveryWindow is how long a volume's recovery may keep
	// failing on transport errors alone (a holder unreachable mid-fetch)
	// before it is treated as unfillable and latched for manual recovery.
	// Default DefaultTransientRecoveryWindow.
	TransientRecoveryWindow time.Duration

	// StaleTimeout is how long a secondary may take to answer a write
	// before the primary marks it Stale instead of stalling clients
	// (default primary.DefaultStaleTimeout, §9).
	StaleTimeout time.Duration

	// OpReplayMaxOps bounds the op-replay fast path (default 4096): a
	// Stale replica whose gap behind the primary is within this many
	// ops is caught up by resending exactly those ops over the live
	// write protocol (runResync's first attempt, see tryOpReplay) —
	// no ZFS snapshot lineage needed, which matters because a replica
	// that has never been through a snapshot-based resync before (e.g.
	// every replica from the volume's original placement) has none. A
	// gap larger than this streams a snapshot instead: one sequential
	// `zfs send` beats thousands of individual request/ack round trips.
	OpReplayMaxOps int

	// OpReplayMaxBytes bounds the op-replay fast path by total payload
	// size (default 256 MiB) — a large number of small ops can still
	// add up to more data than is worth chunking as individual RPCs.
	OpReplayMaxBytes int64

	// ResyncTimeout bounds one runResync attempt end to end (default
	// 10 min) — a backstop, not G6.7's per-test SLA. Without it, a
	// resync that never returns (a `zfs send`/`receive` wedged on the
	// far side, a dead connection with no read/write ever erroring)
	// leaves convInFlight permanently true: EVERY future resync and
	// replica adoption on this node silently stops, forever, with
	// nothing in the logs to say why (found via vol-resync-incremental.nix).
	ResyncTimeout time.Duration

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

	// DetachNBD disconnects the NBD device and removes /dev/exvol/<id>
	// (default: device.Detach shelling out to nbd-client; tests fake
	// it). Every role change that stops local primary service — lease
	// loss, re-election, currency-watchdog step-down, volume delete,
	// process shutdown — must call this: a stale attach is what every
	// "is this node serving as primary?" check relies on (the CLI's
	// `volume inspect`, vol-no-double-primary.nix's §4.7 probe), so
	// leaving one behind makes a fully-recovered volume look forever
	// unserved.
	DetachNBD func(volID, nbdDev string) error

	// ResizeNBD grows an attached NBD device in place, without a
	// reconnect (default: device.ResizeNBD over netlink; tests fake it).
	ResizeNBD func(nbdDev string, sizeBytes int64) error

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
	transient *transientTracker       // recovery failures that were only transport errors
	verifying map[string]bool         // volumes with a currency-watchdog pass in flight
	resyncing map[string]int          // node ID → in-flight resyncs (cap 2 per node)
	oplogs    map[string]*oplog.Store // volID → durable seq->location journal (§4.3 4a), shared across role changes

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
	// snapshotOnly (guarded by Runtime.mu) names replicas whose divergence
	// is not a suffix of the op log — after a restore the primary's zvol
	// jumped backwards without an op — so op-replay would "catch them up"
	// with nothing and leave stale bytes behind.
	snapshotOnly map[string]bool
	// mismatch debounces the currency watchdog (touched only by it).
	mismatch mismatchConfirmer
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
	if opts.StaleTimeout <= 0 {
		opts.StaleTimeout = primary.DefaultStaleTimeout
	}
	if opts.ResyncTimeout <= 0 {
		opts.ResyncTimeout = 10 * time.Minute
	}
	if opts.OpReplayMaxOps <= 0 {
		opts.OpReplayMaxOps = 4096
	}
	if opts.OpReplayMaxBytes <= 0 {
		opts.OpReplayMaxBytes = 256 << 20
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
	if opts.DetachNBD == nil {
		opts.DetachNBD = func(volID, nbdDev string) error {
			return device.Detach(volID, nbdDev, device.ExecRunner{})
		}
	}
	if opts.ResizeNBD == nil {
		opts.ResizeNBD = device.ResizeNBD
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
		transient: newTransientTracker(opts.TransientRecoveryWindow),
		verifying: map[string]bool{},
		resyncing: map[string]int{},
		recv:      map[string]*resyncRecv{},
		oplogs:    map[string]*oplog.Store{},
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
	prims := r.prim
	r.prim = map[string]*volPrimary{}
	r.secs = map[string]*secondary.Secondary{}
	if r.srv != nil {
		r.srv.Close()
		r.srv = nil
	}
	r.mu.Unlock()
	for id, p := range prims {
		p.nsrv.Close()
		// p.coord is deliberately left set, not nil'd: a goroutine that
		// resolved this SAME *volPrimary from r.prim just before the
		// swap above (e.g. adoptNewReplicas/resyncStale, spawned async
		// by convergeReplicas and not tracked/awaited here) may still
		// be mid-flight and read p.coord with no lock of its own — a
		// concurrent nil write here raced that read (found by -race)
		// and, worse, could nil-panic it. Leaving the coordinator
		// object itself in place is safe: its own writer/conns are
		// closed right below, so any straggling use just errors.
		p.held.Abandon()
		if p.coord != nil {
			p.coord.Close() // waits out in-flight writes and retires the pumps, before the writer goes
		}
		p.writer.Close()
		for _, cn := range p.conns {
			cn.Close()
		}
		r.detachDevice(id)
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
				r.demoteIfFenced(spec.ID)
				if err := r.ensurePrimary(ctx, spec, status, zvolPath, devNode); err != nil {
					r.opts.Logger.Warn("primary ensure failed", "vol", id, "err", err)
				} else {
					r.reportSequence(ctx, spec.ID, status)
					r.verifyServingAsync(ctx, spec.ID)
					r.applyResize(spec.ID, spec.SizeBytes)
					r.periodicSnapshot(id, zvolPath)
					r.publishReplicaRoles(ctx, spec.ID)
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
	r.processSnapshotOps(ctx, seen)
	r.processRestoreOps(ctx, seen)
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

// snapshotOp is a user-requested named snapshot (T15 §4.8, G6.13).
type snapshotOp struct {
	Name string `json:"name"`
}

// processSnapshotOps takes a named zvol snapshot for volumes this node
// is primary of. Only the primary acts (the snapshot must reflect the
// currently-serving copy); a non-primary node just leaves the op for
// whichever node holds the role.
func (r *Runtime) processSnapshotOps(ctx context.Context, seen map[string]bool) {
	ops, err := opsOf(ctx, r.opts.St, "snapshot")
	if err != nil {
		return
	}
	for volID, e := range ops {
		if !seen[volID] {
			continue
		}
		var op snapshotOp
		_ = json.Unmarshal(e.Value, &op)
		status, _, err := storage.LoadStatus(ctx, r.opts.St, volID)
		if err != nil || status.Primary != r.opts.NodeID {
			continue // not the primary here — the primary's runtime consumes it
		}
		zp := r.zvolOf[volID]
		if zp == "" {
			continue // zvol not ensured yet; next tick
		}
		if op.Name == "" || strings.HasPrefix(op.Name, resync.SnapPrefix) {
			// The resync- prefix is reserved for the internal
			// @resync-<seq> lineage (retention destroys the oldest past
			// DefaultKeep, and CommonAncestor parses it as a sequence
			// number) — a user snapshot sharing it could be silently
			// destroyed by Retain or misread as a resync checkpoint.
			r.opts.Logger.Warn("snapshot op rejected: name is empty or reserved", "vol", volID, "name", op.Name)
			delOp(ctx, r.opts.St, "snapshot", volID)
			continue
		}
		r.mu.Lock()
		p := r.prim[volID]
		r.mu.Unlock()
		if p == nil || p.coord == nil {
			continue
		}
		if err := p.coord.Flush(); err != nil {
			r.opts.Logger.Warn("snapshot: flush failed", "vol", volID, "err", err)
			continue // retry next tick
		}
		if err := r.zfs.Snapshot(ctx, zp, op.Name); err != nil {
			r.opts.Logger.Warn("snapshot failed", "vol", volID, "name", op.Name, "err", err)
			continue
		}
		delOp(ctx, r.opts.St, "snapshot", volID)
		r.opts.Logger.Info("snapshot taken", "vol", volID, "name", op.Name)
	}
}

// restoreOp is a user-requested rollback to a named snapshot (T15
// §4.8, G6.13).
type restoreOp struct {
	Snapshot string `json:"snapshot"`
}

// processRestoreOps rolls a volume back to a named snapshot on the
// node currently primary for it. The primary's zvol is rolled back in
// place (§9-style destructive, deliberate operation — anything newer
// than the snapshot, including intermediate @resync-* checkpoints, is
// gone); every other replica is then marked Stale via the coordinator
// so the existing resync machinery (§4.3, already proven by
// vol-resync-incremental.nix) reconverges them — zfs send/receive
// diffs are content-based, not chronological, so the normal common-
// ancestor incremental path (or a full send, if none survived the
// rollback) reconciles a rolled-back primary exactly like any other
// resync.
func (r *Runtime) processRestoreOps(ctx context.Context, seen map[string]bool) {
	ops, err := opsOf(ctx, r.opts.St, "restore")
	if err != nil {
		return
	}
	for volID, e := range ops {
		if !seen[volID] {
			continue
		}
		var op restoreOp
		if json.Unmarshal(e.Value, &op) != nil || op.Snapshot == "" {
			delOp(ctx, r.opts.St, "restore", volID)
			continue
		}
		status, _, err := storage.LoadStatus(ctx, r.opts.St, volID)
		if err != nil || status.Primary != r.opts.NodeID {
			continue
		}
		zp := r.zvolOf[volID]
		if zp == "" {
			continue
		}
		r.mu.Lock()
		p := r.prim[volID]
		r.mu.Unlock()
		if p == nil || p.coord == nil {
			continue
		}
		snaps, err := r.zfs.ListSnapshots(ctx, zp)
		if err != nil {
			continue
		}
		found := false
		for _, s := range snaps {
			if s == op.Snapshot {
				found = true
				break
			}
		}
		if !found {
			r.opts.Logger.Error("restore: no such snapshot — refusing", "vol", volID, "snapshot", op.Snapshot)
			delOp(ctx, r.opts.St, "restore", volID)
			continue
		}
		if err := p.coord.Flush(); err != nil {
			r.opts.Logger.Warn("restore: flush failed", "vol", volID, "err", err)
			continue // retry next tick
		}
		if err := r.zfs.Rollback(ctx, zp, op.Snapshot); err != nil {
			r.opts.Logger.Warn("restore: rollback failed", "vol", volID, "err", err)
			continue
		}
		r.mu.Lock()
		if p.snapshotOnly == nil {
			p.snapshotOnly = map[string]bool{}
		}
		for _, id := range p.coord.ReplicaIDs() {
			p.snapshotOnly[id] = true
		}
		r.mu.Unlock()
		for _, id := range p.coord.ReplicaIDs() {
			p.coord.MarkStale(id)
		}
		delOp(ctx, r.opts.St, "restore", volID)
		r.opts.Logger.Info("volume restored", "vol", volID, "snapshot", op.Snapshot)
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
		// Spec.SizeBytes may have grown since creation (G6.14 online
		// resize, grow-only, enforced at the controller's op-CAS layer) —
		// `zfs set volsize` is a cheap idempotent no-op when unchanged,
		// so just always converge every replica's zvol to it here rather
		// than tracking a separate dirty flag.
		if err := r.zfs.Resize(ctx, zvolPath, spec.SizeBytes); err != nil {
			r.opts.Logger.Warn("zvol resize failed", "zvol", zvolPath, "err", err)
		}
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
	// Must happen before r.mu below (held via defer for the rest of
	// this function on the construction path): volOplog takes r.mu
	// itself, and Go's Mutex isn't reentrant. Cheap once cached (a
	// lock+map lookup), so paying it even on the early-return path
	// below is not worth avoiding with a second locking scheme.
	opStore := r.volOplog(context.Background(), volID)

	r.mu.Lock()
	r.zvolOf[volID] = zvolPath
	r.specOf[volID] = spec
	if sec, ok := r.secs[volID]; ok {
		_, receiving := r.recv[volID]
		r.mu.Unlock()
		if sec.HasWriter() || receiving {
			// A live `zfs receive` (see receiveChunk) also leaves the
			// writer nil for its whole duration — indistinguishable from
			// an abandoned stream by HasWriter() alone. Healing here
			// while one is in flight reopens the device out from under
			// `zfs receive -F`, which recreates the dataset object mid-
			// stream: the receive wedges forever, and since it runs
			// inside the primary's convergeReplicas goroutine, THAT
			// never returns either — permanently starving every future
			// resync on this node (found via vol-resync-incremental.nix:
			// a stale replica stayed stale for the rest of the run).
			// reopenLocalDevice re-arms this secondary once the receive
			// actually finishes, success or failure.
			if sec.HasWriter() {
				// G6.14: a healthy, already-open secondary is never
				// reopened, so its writer's enforced bound would
				// otherwise stay stuck at whatever size it had when
				// first attached — replicated writes into a region the
				// primary just grew into would be rejected as
				// out-of-range even though ensureZvol already grew the
				// underlying zvol.
				sec.SetSize(int64(spec.SizeBytes))
			}
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
	// Durable oplog (§4.3 4a), shared with the primary role via
	// volOplog: a daemon restart — or a primary<->secondary role flip
	// on this node — must not forget which sequences the zvol holds,
	// or recovery misjudges the replica and FetchOps can't serve ops it
	// no longer remembers (§4.3 failover deadlocks). The log lives
	// INSIDE the pool's volumes dataset so it shares the zvol's crash
	// domain: whatever txg loss erased zvol data erases the same
	// records here, keeping the metadata truthful.
	if opStore != nil {
		sec.AttachOplogStore(opStore)
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
// secondary's op log — or, when this node currently holds the PRIMARY
// role for volID, its own live replication health (§4.6): a raft-
// independent read of Degraded/ReadOnly status. This is what lets
// `volume inspect` (cmd_volume_ops.go's liveState) show the truth
// even while the cluster's raft control plane has itself lost write
// quorum and cannot persist a status update — the exact case
// enforceReplication's own persisted State would otherwise go stale
// in (controller.go), since both share the same floor(R/2)+1 rule.
func (r *Runtime) querySeq(volID string) (*pb.SeqQueryReply, error) {
	r.mu.Lock()
	sec := r.secs[volID]
	p := r.prim[volID]
	r.mu.Unlock()
	if p != nil && p.coord != nil {
		coord := p.coord
		healthy := 1 + len(coord.ReplicaIDs()) - len(coord.StaleReplicas())
		return &pb.SeqQueryReply{
			VolId: volID, LastSeq: coord.LastSeq(),
			IsPrimary: true, Replication: int32(coord.Replication()), HealthyReplicas: int32(healthy),
		}, nil
	}
	if sec == nil {
		return nil, experrors.New(experrors.KindNotFound, "exvol.runtime.querySeq", "no such volume: "+volID)
	}
	rep := &pb.SeqQueryReply{VolId: volID, LastSeq: sec.LastSeq()}
	for seq, rec := range sec.OpLog() {
		rep.Ops = append(rep.Ops, &pb.SeqInfo{Seq: seq, Crc32C: rec.CRC, Offset: uint64(rec.Offset), Length: uint32(rec.Length)})
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
	if !have {
		// zvolOf is otherwise only populated by ensureSecondary — a
		// primary that was placed there at creation (the common case)
		// and never served as a secondary first left it empty forever,
		// silently no-opping every op that looks it up here: the forced
		// `volume resync` op (processResyncOps, pre-existing) and the
		// new snapshot/restore ops (G6.13) alike (found via
		// vol-snapshot.nix — restore never fired, no log line, no
		// error, just a permanently ignored op).
		r.zvolOf[spec.ID] = zvolPath
	}
	r.mu.Unlock()
	if have {
		return nil
	}

	// A primary must be reachable too, not just secondaries: other
	// nodes' controllers QuerySeq/FetchOps it (recovery/election
	// evidence, `volume diverged`'s probeSeqs, this CLI's own
	// liveState probe) regardless of whether this node ALSO happens to
	// hold a secondary role for some other volume — the previous
	// server-only-on-secondary-role wiring left a pure-primary node's
	// transport listener never started, unreachable even by itself.
	r.mu.Lock()
	srvErr := r.ensureServerLocked()
	r.mu.Unlock()
	if srvErr != nil {
		return fmt.Errorf("replication server: %w", srvErr)
	}

	held, err := r.lm.TryAcquire(ctx, "exvol-vol-"+spec.ID, r.opts.LeaseTTL)
	if err != nil {
		return fmt.Errorf("volume lease held by another primary (%s): %w", describeLease(ctx, r.opts.St, "exvol-vol-"+spec.ID), err)
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

	// Durable oplog (§4.3 4a): the SAME journal a secondary-role stint
	// on this node uses (see ensureSecondary / volOplog). A node's own
	// writes made while primary must not vanish from its reported
	// history if it later restarts or rejoins as a secondary —
	// recovery would misjudge it as behind and never re-level it.
	opStore := r.volOplog(ctx, spec.ID)

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
		recRes, startSeq, err = r.recoverVol(ctx, spec.ID, w, conns, staleReplicas(status), opStore)
		if err != nil {
			w.Close()
			r.lm.Release(ctx, held)
			for _, cn := range conns {
				cn.Close()
			}
			var div *recovery.DivergedError
			var unfill *recovery.UnfillableError
			var transient *recovery.TransientError
			if errors.As(err, &transient) {
				if !r.transient.note(spec.ID, time.Now()) {
					// A holder's link dropped mid-fetch: it may still hold
					// the op. Fail closed (do not serve) but keep retrying.
					r.opts.Logger.Warn("recovery deferred: a holder was unreachable mid-fetch; will retry", "vol", spec.ID, "seq", transient.Seq, "err", transient.Err)
					return fmt.Errorf("volume recovery failed: %w", err)
				}
				// Failing on transport alone for a whole window: no better
				// than a holder that never serves. Latch, as unfillable.
				r.transient.clear(spec.ID)
				unfill = &recovery.UnfillableError{Seq: transient.Seq, Err: transient.Err}
				err = unfill
			}
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
		r.transient.clear(spec.ID)
	}
	var reps []primary.Replica
	for nodeID, conn := range conns {
		reps = append(reps, primary.Replica{NodeID: nodeID, Sender: transport.NewSender(conn, 0, 0)})
	}
	coord := primary.NewAt(spec.ID, spec.Replication, w, reps, held, r.opts.StaleTimeout, startSeq)
	if opStore != nil {
		coord.AttachOplog(opStore)
	}
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
	r.publishOwnPrimaryRow(ctx, spec.ID, startSeq)
	r.opts.Logger.Info("primary serving", "vol", spec.ID, "zvol", zvolPath, "replicas", len(reps), "seq", startSeq)
	return nil
}

// publishOwnPrimaryRow refreshes this node's OWN Placement row to
// Role=Primary/Healthy/LastSeen=now on successful promotion. Election
// (controller.electPrimary) only ever writes status.Primary and
// status.State — nothing else updates the Placement table's Role
// column when a NEW node takes over, so a node that was ALSO marked
// Stale moments earlier (e.g. it was one of several replicas down
// during an outage, then itself got elected once it rejoined) would
// otherwise keep reporting itself Stale forever despite successfully
// serving as primary: `volume inspect`/wait-for-convergence tooling
// would never see it as healthy.
func (r *Runtime) publishOwnPrimaryRow(ctx context.Context, volID string, seq uint64) {
	status, rev, err := storage.LoadStatus(ctx, r.opts.St, volID)
	if err != nil {
		return
	}
	changed := false
	for i := range status.Placement {
		if status.Placement[i].NodeID != r.opts.NodeID {
			continue
		}
		pl := &status.Placement[i]
		if pl.Role != storage.RolePrimary || !pl.Healthy || pl.Sequence != seq {
			pl.Role = storage.RolePrimary
			pl.Healthy = true
			pl.Sequence = seq
			pl.LastSeen = time.Now()
			changed = true
		}
		break
	}
	if !changed {
		return
	}
	if err := storage.CompareAndSwapStatus(ctx, r.opts.St, volID, rev, status); err != nil {
		r.opts.Logger.Warn("publish own primary row failed", "vol", volID, "err", err)
	}
}

// primaryDev returns the volume's serving device, or a not-primary error.
func (r *Runtime) primaryDev(op, volID string) (*device.ExvolDevice, error) {
	r.mu.Lock()
	p := r.prim[volID]
	r.mu.Unlock()
	if p == nil {
		return nil, experrors.New(experrors.KindUnavailable, "exvol.runtime."+op, "not primary for "+volID)
	}
	return p.dev, nil
}

// WriteOp issues one replicated data write through the volume's device
// (lease check, then the §4.3 quorum path). The NBD server is the
// production client path; this is the programmatic ops surface.
func (r *Runtime) WriteOp(volID string, off int64, data []byte) error {
	dev, err := r.primaryDev("WriteOp", volID)
	if err != nil {
		return err
	}
	_, err = dev.WriteAt(data, off)
	return err
}

// ReadOp reads through the volume's device, exactly like an NBD read
// (lease-checked, served from the primary's local zvol).
func (r *Runtime) ReadOp(volID string, off int64, buf []byte) error {
	dev, err := r.primaryDev("ReadOp", volID)
	if err != nil {
		return err
	}
	_, err = dev.ReadAt(buf, off)
	return err
}

// FlushOp issues a replicated flush marker (the client-visible
// durability barrier — data + oplog are fsynced at quorum).
func (r *Runtime) FlushOp(volID string) error {
	dev, err := r.primaryDev("FlushOp", volID)
	if err != nil {
		return err
	}
	return dev.Flush(context.Background())
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

// staleReplicas names the placements whose persisted role is Stale.
func staleReplicas(st storage.Status) map[string]bool {
	out := map[string]bool{}
	for _, pl := range st.Placement {
		if pl.Role == storage.RoleStale {
			out[pl.NodeID] = true
		}
	}
	return out
}

// recoverVol runs the §4.3 failover recovery algorithm (4a–4d) against
// the other replicas over their live transport connections, returning
// the seq the new primary resumes at (step 5). It returns a
// *recovery.DivergedError when the branches have diverged (§9) — the
// caller must NOT serve and must preserve all copies.
func (r *Runtime) recoverVol(ctx context.Context, volID string, w *localwrite.Writer, conns map[string]*transport.Conn, stale map[string]bool, ops *oplog.Store) (recovery.Result, uint64, error) {
	probes := make([]recovery.Probe, 0, len(conns)+1)
	answers := map[string]*pb.SeqQueryReply{} // evidence peers' answers, for the branch check below
	for nodeID, conn := range conns {
		nodeID, conn := nodeID, conn
		if stale[nodeID] {
			// Its copy was already ruled untrustworthy (missed acked ops, or a
			// deposed primary's uncommitted branch): not evidence, just a
			// resync target — reported Stale (4d) like an unreachable one. Its
			// last seq is still asked for: as an upper bound on what it could
			// hold, it lets a sole current replica prove it lacks nothing.
			probe := recovery.Probe{NodeID: nodeID, Reachable: false}
			if q, qerr := querySeqWithin(conn, volID, probeTimeout); qerr == nil {
				probe.Answered, probe.LastSeq = true, q.GetLastSeq()
			}
			probes = append(probes, probe)
			continue
		}
		qrep, err := querySeqWithin(conn, volID, probeTimeout) // step 4a
		if err != nil {
			// Unreachable mid-recovery: step 4d marks it Stale.
			probes = append(probes, recovery.Probe{NodeID: nodeID, Reachable: false})
			continue
		}
		probe, evidence := probeFromAnswer(nodeID, qrep)
		if !evidence {
			// Still serving as primary: report it Stale (4d) and resync it once it demotes.
			probes = append(probes, probe)
			continue
		}
		answers[nodeID] = qrep
		probe.FetchOps = func(_ context.Context, from, to uint64) ([]protocol.WriteOp, error) {
			frep, err := conn.FetchOps(volID, from, to)
			if err != nil {
				return nil, classifyFetchErr(err)
			}
			var ops []protocol.WriteOp
			for _, req := range frep.GetOps() {
				ops = append(ops, protocol.WriteOp{
					Seq: req.GetSeq(), Offset: req.GetOffset(),
					Data: req.GetData(), CRC: req.GetCrc32C(), Flush: req.GetFlush(),
				})
			}
			return ops, nil
		}
		probes = append(probes, probe)
	}
	r.demoteOffBranchPeers(volID, ops, len(conns), probes, answers)
	// The CALLING node's own durable copy may be behind the survivors:
	// it acked ops under the old primary, but a restart (or the §4.3
	// re-election itself) can leave its zvol missing ops the probes
	// report. Recovery 4b only fills the ELECTED node — the controller
	// may have elected THIS node. Before serving, pull every op the
	// local copy lacks from a reachable holder (mirror of 4b).
	localLast := r.localLastSeq(volID)
	selfFetch := newSelfFetcher(func() map[uint64]secondary.OpRecord {
		if sec := r.secOf(volID); sec != nil {
			return sec.OpLog()
		}
		return map[uint64]secondary.OpRecord{}
	}, w).fetch
	sendToReplica := func(_ context.Context, target string, op protocol.WriteOp) error { // 4c
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
	}
	res, err := recovery.Recover(ctx, probes, selfFetch,
		func(_ context.Context, op protocol.WriteOp) error { // 4b
			if op.Flush {
				if err := w.Flush(); err != nil {
					return err
				}
			} else if err := w.WriteAt(op.Data, int64(op.Offset)); err != nil {
				return err
			}
			// The pulled op lands in the local copy via the raw writer,
			// before any coordinator exists to log it (§4.3 4a) — record
			// it into the same durable journal a live write would, or a
			// node that crashes again before its next live write forgets
			// this fill exactly like the bug this journal exists to fix.
			if ops != nil {
				ops.Append(op.Seq, oplog.Record{Offset: int64(op.Offset), Length: len(op.Data), CRC: op.CRC}, op.Flush)
			}
			return nil
		},
		sendToReplica)
	if errors.Is(err, recovery.ErrNoReachableReplica) {
		// No trusted peer. Going offline is the wrong answer when this node
		// provably holds every acked op: serve, and let the automatic resync
		// bring the Stale peers back (writes resume once one has caught up).
		sole, ok, why := recovery.SoleCurrent(probes, r.opts.NodeID, localLast)
		if !ok {
			r.opts.Logger.Warn("recovery: cannot serve as the sole current replica", "vol", volID, "why", why)
			return recovery.Result{}, 0, err
		}
		r.opts.Logger.Warn("recovery: serving as the sole current replica; stale peers will be resynced automatically",
			"vol", volID, "seq", localLast, "stale", sole.Stale)
		res, err = sole, nil
	}
	if err != nil {
		return recovery.Result{}, 0, err
	}
	r.opts.Logger.Info("volume recovered", "vol", volID,
		"primary", res.NewPrimaryID, "seq", res.MaxSeq, "pulled", res.Pulled, "stale", res.Stale)
	for _, f := range res.SyncFailures {
		r.opts.Logger.Warn("4c leveling failed; replica marked stale", "vol", volID,
			"node", f.Target, "op", f.Op, "err", f.Err)
	}
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
				return res, res.MaxSeq, recovery.FetchFailure(seq, err)
			}
			if op.Flush {
				err = w.Flush()
			} else {
				err = w.WriteAt(op.Data, int64(op.Offset))
			}
			if err == nil && ops != nil {
				ops.Append(op.Seq, oplog.Record{Offset: int64(op.Offset), Length: len(op.Data), CRC: op.CRC}, op.Flush)
			}
			if err != nil {
				return res, res.MaxSeq, fmt.Errorf("local fill: apply op %d: %w", seq, err)
			}
		}
		r.opts.Logger.Info("local copy filled from survivors", "vol", volID,
			"from", localLast, "to", res.MaxSeq)
	}
	// The caller's own durable oplog is evidence too: probes can ALL
	// under-report (the only secondary of the acking quorum crashed;
	// an ex-primary's reboots lose its coordinator memory but NOT its
	// durable oplog). VM run 8: the elected node held ops 1–10 in its
	// own oplog, every probe reported 0/unreachable, and it resumed at
	// seq 0 — re-issuing sequence numbers that already existed. A
	// resume point below the local oplog is never honest.
	if localLast > res.MaxSeq {
		r.opts.Logger.Warn("recovery resume raised to local oplog", "vol", volID,
			"probe_max", res.MaxSeq, "local_last", localLast)
		// 4c leveled every reachable replica up to the OLD (too-low)
		// res.MaxSeq before this correction existed — a replica whose
		// own probed LastSeq happened to already meet that ceiling was
		// certified "current" and skipped, even though the caller's own
		// copy (just proven ahead) has ops beyond it. Re-level the tail
		// now that the true ceiling is known, or those ops NEVER reach
		// that replica: nothing else re-probes or re-triggers a resync
		// for a replica that was never marked stale.
		alreadyStale := map[string]bool{}
		for _, id := range res.Stale {
			alreadyStale[id] = true
		}
		for i := range probes {
			p := probes[i]
			if !p.Reachable || p.LastSeq >= localLast || alreadyStale[p.NodeID] {
				continue
			}
			from := p.LastSeq
			if from < res.MaxSeq {
				from = res.MaxSeq // already leveled to here by 4c above
			}
			leveled := true
			for seq := from + 1; seq <= localLast; seq++ {
				op, ferr := selfFetch(ctx, seq-1, seq)
				if ferr != nil || len(op) != 1 {
					r.opts.Logger.Warn("re-level tail: self-fetch failed", "vol", volID,
						"node", p.NodeID, "seq", seq, "err", ferr)
					leveled = false
					break
				}
				if serr := sendToReplica(ctx, p.NodeID, op[0]); serr != nil {
					r.opts.Logger.Warn("re-level tail: send to replica failed", "vol", volID,
						"node", p.NodeID, "seq", seq, "err", serr)
					leveled = false
					break
				}
			}
			if !leveled {
				res.Stale = append(res.Stale, p.NodeID)
			}
		}
		return res, localLast, nil
	}
	return res, res.MaxSeq, nil
}

// verifyServingTimeout bounds one watchdog pass, including its network I/O.
const verifyServingTimeout = 10 * time.Second

// verifyServingAsync runs the currency watchdog off the reconcile tick, one
// pass per volume at a time. It dials a replica and does two round trips;
// inline, a slow or blackholed replica froze the tick (status publication,
// demotion checks) for as long as that replica took to answer.
func (r *Runtime) verifyServingAsync(ctx context.Context, volID string) {
	r.mu.Lock()
	if r.verifying[volID] {
		r.mu.Unlock()
		return
	}
	r.verifying[volID] = true
	r.mu.Unlock()
	go func() {
		defer func() {
			r.mu.Lock()
			delete(r.verifying, volID)
			r.mu.Unlock()
		}()
		vctx, cancel := context.WithTimeout(ctx, verifyServingTimeout)
		defer cancel()
		r.verifyServingCurrent(vctx, volID)
	}()
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
	stale := map[string]bool{}
	for _, id := range p.coord.StaleReplicas() {
		stale[id] = true
	}
	var conn string
	for _, id := range p.coord.ReplicaIDs() {
		// A replica the coordinator itself already knows is Stale (e.g.
		// mid-resync after a restore, G6.13) is EXPECTED to disagree
		// with the primary's current content — that is what Stale
		// means, not evidence of corruption. Comparing against one
		// here previously stepped the primary down right after a
		// legitimate restore, whose recovery-on-repromotion then
		// pulled the stale replica's pre-restore bytes back in,
		// silently undoing it (found via vol-snapshot.nix).
		if stale[id] {
			continue
		}
		if c := p.conns[id]; c != nil {
			conn = id
			break
		}
	}
	if conn == "" {
		return // no live current replica to compare against; 4d handles isolation
	}
	// NOTE: the pump and the recovery paths own the shared conns; a
	// transport Conn is single-submitter, so the watchdog dials its own
	// short-lived connection per verify instead of reusing p.conns.
	probe, err := r.dialReplica(ctx, conn)
	if err != nil {
		return // availability first: a failed dial is not divergence
	}
	defer probe.Close()
	// transport.Conn takes no context: closing it is how a deadline (or a
	// shutdown) interrupts a QuerySeq/FetchOps stuck on a silent peer.
	defer context.AfterFunc(ctx, func() { _ = probe.Close() })()
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
	if supersededByLaterOp(qrep.GetOps(), seq, op.GetOffset(), uint64(len(op.GetData()))) {
		p.mismatch.observe(seq, false)
		return // rewritten since (e.g. fs metadata): the two copies are read at different instants
	}
	buf := make([]byte, len(op.GetData()))
	if _, err := p.writer.ReadAt(buf, int64(op.GetOffset())); err != nil {
		r.opts.Logger.Error("currency watchdog: primary cannot re-read its own zvol — stepping down",
			"vol", volID, "seq", seq, "err", err)
		r.stopPrimary(volID)
		return
	}
	mismatch := crc32c(buf) != crc32c(op.GetData())
	if !p.mismatch.observe(seq, mismatch) {
		if mismatch {
			p.verifySeq = seq - 1 // look at this same op again next tick
		}
		return
	}
	r.opts.Logger.Error("currency watchdog: primary zvol diverges from the replica's durable record — stepping down for re-recovery",
		"vol", volID, "seq", seq, "off", op.GetOffset(), "replica", conn)
	r.stopPrimary(volID)
}

// mismatchConfirmer debounces the watchdog: the primary and replica
// copies are read a moment apart, so a write in flight to the range can
// make them briefly disagree. Real divergence persists.
type mismatchConfirmer struct{ pending uint64 }

// observe records one check of op seq and reports whether a mismatch is
// now confirmed (the same op mismatched on two consecutive checks).
func (m *mismatchConfirmer) observe(seq uint64, mismatch bool) bool {
	if !mismatch {
		if m.pending == seq {
			m.pending = 0
		}
		return false
	}
	if m.pending == seq {
		return true
	}
	m.pending = seq
	return false
}

// supersededByLaterOp reports whether any op after seq wrote into
// [off, off+length): the primary's current bytes there are then expected
// to differ from op seq's payload, so comparing them proves nothing.
func supersededByLaterOp(claims []*pb.SeqInfo, seq, off, length uint64) bool {
	for _, c := range claims {
		if c.GetSeq() <= seq || c.GetLength() == 0 {
			continue
		}
		if c.GetOffset() < off+length && off < c.GetOffset()+uint64(c.GetLength()) {
			return true
		}
	}
	return false
}

// applyResize grows an already-serving primary's live device to match
// the current spec (G6.14, online — no unmount). ensureZvol already
// grew the local zvol itself; this is the part that only matters for
// an ALREADY-running primary (ensurePrimary only ever reads spec.
// SizeBytes once, at promotion — it short-circuits on every later tick
// via the `have` guard) — the writer's own enforced bound and the live
// NBD export both need to be told about the new size explicitly.
func (r *Runtime) applyResize(volID string, sizeBytes uint64) {
	r.mu.Lock()
	p := r.prim[volID]
	r.mu.Unlock()
	if p == nil || p.dev == nil {
		return
	}
	newSize := int64(sizeBytes)
	if p.dev.Size() == newSize {
		return
	}
	oldSize := p.dev.Size()
	p.dev.SetSize(newSize)
	if p.writer != nil {
		p.writer.SetSize(newSize)
	}
	// Server bounds grow first so I/O to the new range is accepted the
	// moment the kernel sees it. On failure roll back: the size check
	// above would otherwise skip every later retry.
	if err := r.opts.ResizeNBD(r.nbdDevFor(volID), newSize); err != nil {
		p.dev.SetSize(oldSize)
		if p.writer != nil {
			p.writer.SetSize(oldSize)
		}
		r.opts.Logger.Error("resize: kernel nbd resize failed; will retry", "vol", volID, "err", err)
		return
	}
	r.opts.Logger.Info("volume resized live", "vol", volID, "sizeBytes", sizeBytes)
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

// demoteIfFenced stops a primary whose coordinator fenced itself: a write
// it had applied locally never reached quorum, so its zvol holds
// uncommitted bytes and recovery must re-decide before it serves again.
func (r *Runtime) demoteIfFenced(volID string) {
	r.mu.Lock()
	p, ok := r.prim[volID]
	r.mu.Unlock()
	if !ok || p.coord == nil || !p.coord.Fenced() {
		return
	}
	r.opts.Logger.Warn("primary fenced: a write missed quorum — demoting to re-run recovery", "vol", volID)
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
	if p.coord != nil {
		p.coord.Close()
	}
	p.writer.Close()
	for _, cn := range p.conns {
		cn.Close()
	}
	r.adoptOwnWrites(volID)
	r.detachDevice(volID)
}

// adoptOwnWrites raises the local secondary's last seq to what this node wrote
// as primary (the shared oplog has them, its protocol counter does not).
func (r *Runtime) adoptOwnWrites(volID string) {
	if sec := r.secOf(volID); sec != nil {
		sec.AttachOplogStore(r.volOplog(context.Background(), volID))
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

// volOplog returns this node's durable seq->location journal for volID
// (§4.3 4a), opening and caching it on first use. Cached rather than
// reopened per call: ensurePrimary retries its ENTIRE bring-up from
// scratch on every failed attempt (unlike ensureSecondary, which sets
// up once and then short-circuits), so an uncached open would run a
// real zfs subprocess + file open on every retry of a volume stuck
// failing recovery — wasted work, and in one chaos-test run measurably
// disruptive to the tight retry loop it sits in. One Store per volID
// for the life of the process also means a primary<->secondary role
// flip on this node shares the exact same in-memory + on-disk history
// instead of two independent views that could drift.
func (r *Runtime) volOplog(ctx context.Context, volID string) *oplog.Store {
	r.mu.Lock()
	if st, ok := r.oplogs[volID]; ok {
		r.mu.Unlock()
		return st
	}
	r.mu.Unlock()

	mp, mpErr := r.zfs.Mountpoint(ctx, r.opts.Pool+"/volumes")
	if mpErr != nil || mp == "" {
		r.opts.Logger.Warn("durable oplog unavailable (memory-only)", "vol", volID, "err", mpErr)
		return nil
	}
	dir := mp + "/.oplogs"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		r.opts.Logger.Warn("oplog dir create failed (memory-only)", "vol", volID, "err", err)
		return nil
	}
	st, err := oplog.Open(fmt.Sprintf("%s/%s.oplog", dir, volID))
	if err != nil {
		r.opts.Logger.Warn("oplog store open failed (memory-only)", "vol", volID, "err", err)
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.oplogs[volID]; ok {
		// Lost a race with a concurrent caller — keep the winner, close
		// the redundant handle.
		st.Close()
		return existing
	}
	r.oplogs[volID] = st
	return st
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
		storage.ClearAutoLatch(ctx, r.opts.St, volID) // divergence is for a human, never self-lifting
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

// demoteOffBranchPeers turns peers that a quorum contradicts (see offBranchPeers)
// into non-evidence, like Stale ones: their conflicting tail is resynced away
// instead of being reported as a split brain that takes the volume offline.
func (r *Runtime) demoteOffBranchPeers(volID string, ops *oplog.Store, peerCount int, probes []recovery.Probe, answers map[string]*pb.SeqQueryReply) {
	if ops == nil || len(answers) == 0 {
		return
	}
	replication := peerCount + 1
	r.mu.Lock()
	if spec, ok := r.specOf[volID]; ok && spec.Replication > 0 {
		replication = spec.Replication
	}
	r.mu.Unlock()
	off := offBranchPeers(ops.Snapshot(), answers, replication/2+1)
	for i := range probes {
		id := probes[i].NodeID
		if !off[id] {
			continue
		}
		r.opts.Logger.Warn("recovery: replica holds a branch a quorum contradicts (a deposed primary's uncommitted tail); it will be resynced, not treated as a split brain", "vol", volID, "replica", id)
		probes[i] = recovery.Probe{NodeID: id, Answered: true, LastSeq: probes[i].LastSeq}
	}
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

// detachDevice best-effort tears down this node's NBD attach for volID
// (nbd-client -d + the /dev/exvol/<id> symlink). Best-effort: a failed
// detach is logged, not fatal — the volume's already off the write
// path by the time every caller reaches this, and retrying forever
// would block reconcile on a wedged nbd-client.
func (r *Runtime) detachDevice(volID string) {
	nbdDev := r.nbdDevFor(volID)
	if err := r.opts.DetachNBD(volID, nbdDev); err != nil {
		r.opts.Logger.Warn("nbd detach failed", "vol", volID, "dev", nbdDev, "err", err)
	}
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
	p, wasPrimary := r.prim[volID]
	if wasPrimary {
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
	if wasPrimary {
		p.nsrv.Close()
		p.held.Abandon()
		p.writer.Close()
		for _, cn := range p.conns {
			cn.Close()
		}
		r.detachDevice(volID)
	}
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
	for id := range r.secs {
		if !seen[id] {
			delete(r.secs, id)
			delete(r.zvolOf, id)
		}
	}
	gone := map[string]*volPrimary{}
	for id, p := range r.prim {
		if !seen[id] {
			gone[id] = p
			delete(r.prim, id)
		}
	}
	r.mu.Unlock()
	for id, p := range gone {
		p.nsrv.Close()
		p.held.Abandon()
		p.writer.Close()
		for _, cn := range p.conns {
			cn.Close()
		}
		r.detachDevice(id)
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
			// Clear the in-flight marker and re-arm the local device: a
			// write failure here means the stream is dead (broken pipe to
			// a `zfs receive` that already exited, or similar) — leaving
			// r.recv set would permanently block ensureSecondary's heal
			// path (it now defers to an in-flight receive, see there) for
			// a stream that is never coming back.
			r.mu.Lock()
			delete(r.recv, volID)
			r.mu.Unlock()
			r.reopenLocalDevice(volID)
			return nil, err
		}
	} else {
		rp.pw.Close() // EOF: zfs receive finalizes the image
		err := <-rp.done
		r.reopenLocalDevice(volID)
		if err != nil {
			std := ""
			if rp.stderr != nil {
				std = rp.stderr.String()
			}
			if strings.Contains(std, "destination has snapshots") {
				// A FULL stream requires a snapshot-free destination —
				// zfs refuses to overwrite snapshot history. The source
				// full-sent precisely because none of OUR @resync-*
				// snapshots matched its list, so they are worthless as
				// common ancestors: destroy them and let the source's
				// convergence retry succeed. (Non-resync snapshots would
				// still block — the receive then keeps failing loudly;
				// wiping a user snapshot here would be data loss.)
				r.destroyResyncSnaps(volID)
			}
			if std != "" {
				r.opts.Logger.Warn("zfs receive failed", "vol", volID, "stderr", std)
			}
			return nil, fmt.Errorf("zfs receive: %w", err)
		}
	}
	ack := make([]byte, 8)
	binary.BigEndian.PutUint64(ack, uint64(count))
	return ack, nil
}

// destroyResyncSnaps clears this node's @resync-* snapshots for volID
// (the full-stream-receive precondition; see receiveChunk).
func (r *Runtime) destroyResyncSnaps(volID string) {
	r.mu.Lock()
	zp := r.zvolOf[volID]
	r.mu.Unlock()
	if zp == "" {
		return
	}
	ctx := context.Background()
	snaps, err := r.zfs.ListSnapshots(ctx, zp)
	if err != nil {
		r.opts.Logger.Warn("resync snap destroy: list failed", "vol", volID, "err", err)
		return
	}
	for _, s := range snaps {
		if _, ok := resync.ParseSeq(s); !ok {
			continue
		}
		if err := r.zfs.DestroySnapshot(ctx, zp, s); err != nil {
			r.opts.Logger.Warn("resync snap destroy failed", "vol", volID, "snap", s, "err", err)
		}
	}
	r.opts.Logger.Info("destroyed local resync snapshots for full-stream receive", "vol", volID)
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
	r.mu.Unlock()
	err := p.coord.AtSeq(func(seq uint64) error {
		if seq == 0 {
			return nil // nothing written yet
		}
		_, err := resync.EnsureSnapshot(context.Background(), r.zfs, zvolPath, seq, r.opts.SnapshotKeep)
		return err
	})
	if err != nil {
		r.opts.Logger.Warn("resync snapshot failed", "vol", volID, "err", err)
	}
}

// publishReplicaRoles keeps the published roles equal to what this primary's
// coordinator knows (see reconcileRoles), so a lost or skipped status write
// heals on the next tick instead of leaving a replica published Stale forever.
func (r *Runtime) publishReplicaRoles(ctx context.Context, volID string) {
	r.mu.Lock()
	p := r.prim[volID]
	r.mu.Unlock()
	if p == nil || p.coord == nil {
		return
	}
	// A dead pump's failure otherwise only gets processed at the top of
	// the NEXT write (replicate()'s own drainResults) — on an idle
	// volume that may never come, leaving this replica un-detected as
	// Stale (and, worse, still counted toward quorum) indefinitely.
	p.coord.DrainResults()
	stale, live := map[string]bool{}, map[string]bool{}
	for _, id := range p.coord.StaleReplicas() {
		stale[id] = true
	}
	for _, id := range p.coord.ReplicaIDs() {
		live[id] = true
	}
	resyncing := func(id string) bool { return r.resyncInFlight(id) > 0 }
	err := storage.UpdateStatus(ctx, r.opts.St, volID, func(st *storage.Status) bool {
		return reconcileRoles(st.Placement, r.opts.NodeID, stale, live, resyncing)
	})
	if err != nil {
		r.opts.Logger.Warn("publishing replica roles failed; retrying next tick", "vol", volID, "err", err)
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

// tryOpReplay is runResync's fast path: a Stale replica that was
// live-current before a brief outage holds a true PREFIX of the
// primary's durable history, so the gap can be closed by resending
// exactly the ops it missed over the live write protocol (protocol.go
// Handle) — no `zfs send`/`receive` at all. This matters because a
// replica that has never been through a snapshot-based resync (every
// replica from the volume's original placement, until its first ever
// staleness) holds no @resync-<seq> snapshot, so resync.CommonAncestor
// can never find one and the ZFS path is unconditionally a full send —
// for exactly the case this fast path targets.
//
// Returns done=true once it has decided the outcome (caught up, or hit
// a real failure) — the caller must NOT also run the ZFS path then.
// done=false means op-replay does not apply here (gap too large, or
// the primary's own oplog can't serve it) — fall back to a snapshot
// stream.
func (r *Runtime) tryOpReplay(ctx context.Context, volID, target string, p *volPrimary, status storage.Status) (done bool, err error) {
	primarySeq := p.coord.LastSeq()

	// Connectivity failures here are NOT a signal that op-replay doesn't
	// apply — falling through to the ZFS path would commit this whole
	// resync cycle to a full snapshot send over the same unreachable-or-
	// not-yet-ready target, which the ZFS path's own dial would likely
	// hit too. The one exception observed in practice: a replica whose
	// daemon JUST restarted can have its mesh address reachable (exp0 up)
	// before its transport listener is bound — dialReplica's own retries
	// don't always outlast that window. Surfacing the error here (done)
	// lets the NEXT reconcile tick retry the fast path fresh, instead of
	// silently downgrading a brief startup race into an unnecessary full
	// resync of the whole volume.
	conn, derr := r.dialReplica(ctx, target)
	if derr != nil {
		return true, fmt.Errorf("op-replay: dial %s: %w", target, derr)
	}
	defer conn.Close()

	qrep, qerr := conn.QuerySeq(volID)
	if qerr != nil {
		return true, fmt.Errorf("op-replay: querySeq %s: %w", target, qerr)
	}
	targetSeq := qrep.GetLastSeq()
	var primaryLog map[uint64]oplog.Record
	if log := r.volOplog(ctx, volID); log != nil {
		primaryLog = log.Snapshot()
	}
	switch opReplayVerdictFor(qrep, primarySeq, primaryLog) {
	case verdictDefer:
		r.opts.Logger.Info("op-replay deferred: target still serves as primary; retrying after it demotes", "vol", volID, "target", target)
		return true, fmt.Errorf("op-replay: %s still serves as primary", target)
	case verdictSnapshot:
		return false, nil // ahead of us, or its tail is another branch (a deposed primary's): only a snapshot discards it
	}
	if !claimsServeable(ctx, conn, volID, target, qrep) {
		r.opts.Logger.Warn("op-replay refused: the replica cannot serve the ops its oplog claims; rebuilding it from a snapshot", "vol", volID, "target", target)
		return false, nil
	}

	gap := primarySeq - targetSeq
	if gap > uint64(r.opts.OpReplayMaxOps) {
		return false, nil // too many ops to chat one RPC at a time; stream a snapshot instead
	}

	var ops []protocol.WriteOp
	if gap > 0 {
		var ferr error
		ops, ferr = p.coord.FetchOps(targetSeq, primarySeq)
		if ferr != nil {
			return false, nil // primary's own oplog can't serve the range (rotated out) — fall back
		}
		var totalBytes int64
		for _, op := range ops {
			totalBytes += int64(len(op.Data))
		}
		if totalBytes > r.opts.OpReplayMaxBytes {
			return false, nil
		}
		for _, op := range ops {
			if serr := conn.Send(volID, op); serr != nil {
				return true, fmt.Errorf("op-replay: send op %d to %s: %w", op.Seq, target, serr)
			}
			rep, rerr := conn.Recv()
			if rerr != nil {
				return true, fmt.Errorf("op-replay: recv ack for op %d from %s: %w", op.Seq, target, rerr)
			}
			if !rep.ACK {
				// The replica's own state disagrees with what its
				// QuerySeq just reported (raced by a concurrent write,
				// or a claim the local copy can't back) — the snapshot
				// path re-establishes ground truth honestly.
				return false, nil
			}
		}
	}

	// Re-admit with a fresh sender (the op-replay connection was
	// single-purpose), replaying whatever was written meanwhile.
	if aerr := r.admitCaughtUp(ctx, p, volID, target, primarySeq); aerr != nil {
		return true, fmt.Errorf("op-replay: %w", aerr)
	}

	r.publishResynced(ctx, volID, target, primarySeq)
	r.opts.Logger.Info("op-replay resync complete", "vol", volID, "target", target,
		"seq", primarySeq, "ops", gap)
	return true, nil
}

// runResync streams the primary's latest @resync-<seq> snapshot into a
// stale replica incrementally (common-ancestor send -i) or fully (with
// the spec-required WARNING), then re-admits it to quorum.
func (r *Runtime) runResync(ctx context.Context, volID, target, zvolPath string, status storage.Status) error {
	ctx, cancel := context.WithTimeout(ctx, r.opts.ResyncTimeout)
	defer cancel()

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

	r.mu.Lock()
	snapshotOnly := p.snapshotOnly[target]
	r.mu.Unlock()
	if !snapshotOnly {
		if done, err := r.tryOpReplay(ctx, volID, target, p, status); done {
			return err
		}
	}

	// seq == 0 (fresh volume, nothing written): there is no @resync-0 to
	// reference — take one anyway and full-send the (empty) zvol. The
	// old behavior ("has no writes to resync") retried forever, keeping
	// the replica stale indefinitely on an idle volume (the observed
	// livelock). Taken under the write lock so the snapshot holds every
	// op up to its name (see Coordinator.AtSeq).
	if err := p.coord.AtSeq(func(seq uint64) error {
		_, err := resync.EnsureSnapshot(ctx, r.zfs, zvolPath, seq, r.opts.SnapshotKeep)
		return err
	}); err != nil {
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

	// Re-admit the replica on a fresh FOREGROUND connection (the resync
	// one was throttled/DSCP-marked), replaying ops written since the snapshot.
	if err := r.admitCaughtUp(ctx, p, volID, target, out.Adopt); err != nil {
		return fmt.Errorf("re-admit resynced replica: %w", err)
	}

	r.publishResynced(ctx, volID, target, out.Adopt)
	r.mu.Lock()
	delete(p.snapshotOnly, target)
	r.mu.Unlock()
	r.opts.Logger.Info("resync complete", "vol", volID, "target", target,
		"seq", out.Adopt, "incremental", !out.Full, "bytes", out.Bytes)
	return nil
}

// publishResynced records that target is a Secondary again, durable at seq: only
// its own entry changes, on the current record, retried against other writers.
func (r *Runtime) publishResynced(ctx context.Context, volID, target string, seq uint64) {
	err := storage.UpdateStatus(ctx, r.opts.St, volID, func(st *storage.Status) bool {
		for i := range st.Placement {
			if pl := &st.Placement[i]; pl.NodeID == target {
				pl.Role, pl.Healthy, pl.Sequence, pl.LastSeen = storage.RoleSecondary, true, seq, time.Now()
				return true
			}
		}
		return false
	})
	if err != nil {
		r.opts.Logger.Warn("resync outcome not recorded; the next role reconcile will publish it", "vol", volID, "target", target, "err", err)
	}
}

// catchUpLockedTail is how many ops may remain when admitCaughtUp takes
// the write lock for the final replay; bigger tails are drained unlocked.
const catchUpLockedTail = 64

// admitCaughtUp re-admits a resynced replica to the live fan-out without
// a sequence gap. `sent` is the last seq the resync delivered; ops
// written since are replayed (unlocked while many, then the last few
// under the coordinator's write lock) so none slip past between the
// replay and the replica's pump starting.
func (r *Runtime) admitCaughtUp(ctx context.Context, p *volPrimary, volID, target string, sent uint64) error {
	cu, err := r.dialReplica(ctx, target)
	if err != nil {
		return fmt.Errorf("dial %s for catch-up: %w", target, err)
	}
	defer cu.Close()
	deliver := func(op protocol.WriteOp) error {
		if err := cu.Send(volID, op); err != nil {
			return err
		}
		rep, err := cu.Recv()
		if err != nil {
			return err
		}
		if !rep.ACK {
			return fmt.Errorf("replica %s nacked catch-up op %d: %s", target, op.Seq, rep.Reason)
		}
		return nil
	}
	for cur := p.coord.LastSeq(); cur > sent+catchUpLockedTail; cur = p.coord.LastSeq() {
		ops, ferr := p.coord.FetchOps(sent, cur)
		if ferr != nil {
			return fmt.Errorf("catch-up fetch (%d,%d]: %w", sent, cur, ferr)
		}
		for _, op := range ops {
			if err := deliver(op); err != nil {
				return err
			}
		}
		sent = cur
	}
	fg, err := r.dialReplica(ctx, target)
	if err != nil {
		return fmt.Errorf("re-dial %s: %w", target, err)
	}
	if err := p.coord.ReviveCaughtUp(target, transport.NewSender(fg, 0, 0), sent, deliver); err != nil {
		_ = fg.Close() // nothing was admitted; the connection is unused
		return err
	}
	r.opts.Logger.Info("replica re-admitted to the live fan-out", "vol", volID, "target", target, "caught_up_to", sent)
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
