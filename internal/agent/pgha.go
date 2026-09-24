package agent

// PostgreSQL primary election (PHASE-05-TASKS.md Stream A, D1): scans
// block placements for db/postgres instances this node hosts a
// replica of and runs the lease-gated election controller
// (internal/blocks/pgha) against them. Parallel in shape to
// vipPass/vipLoop, but the controller does its own store I/O (lease
// acquire/inspect) rather than needing platform seams.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/blocks/controller"
	"github.com/expanse/expanse/internal/blocks/pgha"
	"github.com/expanse/expanse/internal/blocks/runtime/systemd"
	"github.com/expanse/expanse/internal/cluster/lease"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// pgScanInterval matches vipScanInterval: a role decision, once
// written, is never revisited, so there is no failover-latency budget
// to tune this against — it only bounds how quickly a freshly placed
// replica gets electioned.
const pgScanInterval = vipScanInterval

// pgDefaultPort is db/postgres's defaults.yaml port value, used only
// when a deployed instance's spec.config omits it — admission (V19)
// validates spec.config as submitted, it does not merge defaults.yaml
// first (catalog.Defaults is not consumed anywhere in that path), and
// no generic config+defaults merge exists yet in the block build
// path either. Tracks nix/blocks/db/postgres/defaults.yaml; a mismatch
// here would misroute replica connections, not fail closed, so keep
// it in sync by hand until that generic merge exists.
const pgDefaultPort = 5432

func (a *Agent) initPG() {
	if a.pgCtl != nil {
		return
	}
	a.pgCtl = pgha.New(pgha.Config{
		Leases:      lease.NewManager(a.store, a.cfg.NodeID),
		Self:        a.cfg.NodeID,
		ResolveAddr: a.lookupNodeIP,
		Logger:      a.logger.With("component", "pgha"),
	})
}

func (a *Agent) pgLoop(ctx context.Context) {
	a.initPG()
	tick := time.NewTicker(pgScanInterval)
	defer tick.Stop()
	for {
		a.pgCtl.Pass(ctx, a.scanPostgresInstances(ctx))
		select {
		case <-ctx.Done():
			a.pgCtl.Stop()
			return
		case <-tick.C:
		}
	}
}

// scanPostgresInstances lists /blocks/, keeping only db/postgres
// instances with a RUNNING placement on this node. A failed list
// resolves to "nothing to reconcile this pass" — the same
// don't-flap-on-a-blip choice vipPass makes (internal/agent/vip.go):
// an already-elected instance has nothing to lose from a skipped
// scan, and a not-yet-elected one simply waits one more tick.
func (a *Agent) scanPostgresInstances(ctx context.Context) map[string]pgha.Instance {
	out := map[string]pgha.Instance{}
	ents, err := a.store.List(ctx, store.Key("/blocks/"))
	if err != nil {
		return out
	}
	for _, e := range ents {
		k := string(e.Key)
		if len(k) > 7 && k[len(k)-7:] == "/status" {
			continue
		}
		var b pb.Block
		if err := proto.Unmarshal(e.Value, &b); err != nil {
			continue
		}
		if b.GetSpec().GetType() != pgha.BlockType {
			continue
		}
		if se, err := a.store.Get(ctx, store.Key(k+"/status")); err == nil {
			var st pb.BlockStatus
			if err := proto.Unmarshal(se.Value, &st); err == nil {
				b.Status = &st
			}
		}
		// Any live placement counts, not just RUNNING: RUNNING itself
		// requires the workload to already be up, but db/postgres's own
		// workload can only ever start once pgha has written a role
		// decision -- gating the scan on RUNNING is a real chicken-and-
		// egg deadlock (found running the X1 VM test: the only reason
		// election ever succeeded at all was systemd transiently
		// reporting a crash-looping unit "active" long enough for an
		// unrelated reconcile tick to catch it, not a real readiness
		// signal a losing replica could rely on).
		idx := int32(-1)
		for _, pl := range b.GetStatus().GetPlacements() {
			if pl.GetNodeId() == a.cfg.NodeID && pl.GetReplicaIndex() >= 0 && pl.GetPhase() != pb.Phase_LOST {
				idx = pl.GetReplicaIndex()
				break
			}
		}
		if idx < 0 {
			continue
		}
		ref := k[len("/blocks/"):]
		mount := a.pgReplicaMountPath(ctx, ref, int(idx))
		if mount == "" {
			a.logger.Warn("pgha: replica's real mount path not wired yet, retrying next pass",
				"block", ref, "node", a.cfg.NodeID, "index", idx)
			continue
		}
		port := int32(pgDefaultPort)
		if v, ok := b.GetSpec().GetConfig().GetFields()["port"]; ok {
			port = int32(v.GetNumberValue())
		}
		out[ref] = pgha.Instance{BlockRef: ref, MountPath: mount, Port: port}
	}
	return out
}

// pgReplicaMountPath resolves a replica's REAL host mount path: the same
// one bridge.go's replicaSpec hands the workload via "--mount", not the
// block's own declared spec.storage[].mountPath (a sandbox-only virtual
// path meaningless to this unsandboxed agent process — a bug found
// running the X1 VM test: pgha was writing its role file under the
// declared path, a plain, unrelated directory on the node's root
// filesystem, while the workload read it from the real per-replica
// volume, so the two never actually met). Reads the desired-state
// record bridge.go already wrote for this node+replica rather than
// re-deriving the volume name/id chain independently, so there is only
// one place that computation can drift.
func (a *Agent) pgReplicaMountPath(ctx context.Context, blockRef string, idx int) string {
	slash := strings.Index(blockRef, "/")
	if slash < 0 {
		return ""
	}
	ns, name := blockRef[:slash], blockRef[slash+1:]
	key := a.recon.DesiredPrefix() + store.Key(controller.ReplicaResourceID(ns, name, idx))
	e, err := a.store.Get(ctx, key)
	if err != nil {
		return ""
	}
	_, payload, ok := bytes.Cut(e.Value, []byte("\n"))
	if !ok {
		return ""
	}
	var spec systemd.Spec
	if err := json.Unmarshal(payload, &spec); err != nil {
		return ""
	}
	for i := 0; i+1 < len(spec.Args); i++ {
		if spec.Args[i] != "--mount" {
			continue
		}
		if _, path, ok := strings.Cut(spec.Args[i+1], "="); ok {
			return path
		}
	}
	return ""
}
