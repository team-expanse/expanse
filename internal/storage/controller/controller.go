// Package controller is the leader-side volume controller. One instance runs on
// the Raft leader (an IsLeader gate checked every tick). It plans; the node
// runtimes (volume.Node) act. Responsibilities:
//
//   - placement of queued creates (place.go);
//   - primary election among healthy replicas (election.go). DRBD quorum and
//     the volume lease make promotion safe, so election only chooses who tries;
//   - deletion: mark Deleting, wait for every node to drop its placement row,
//     then release the allocation and drop the records (delete.go);
//   - the volume's derived state and under-replication alerts (§4.6).
//
// Volumes in StateNeedsManualRecovery are never touched: a human decides.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/quantity"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/storage/drbd"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	pbproto "google.golang.org/protobuf/proto"
)

// Options configures the controller.
type Options struct {
	NodeID string
	St     store.Store
	Logger *slog.Logger

	// IsLeader gates every mutating action: only the Raft leader plans.
	IsLeader func() bool

	// Alert receives under-replication events (§4.6 "alert if it cannot be met").
	Alert func(AlertEvent)

	// Interval is the reconcile cadence (default 5 s).
	Interval time.Duration

	// Alloc hands out each volume's DRBD minor, port and node-ids at creation
	// and takes them back at deletion.
	Alloc *drbd.Allocator
}

// AlertEvent is a controller-raised alert.
type AlertEvent struct {
	VolID  string
	Kind   string // "under-replication"
	Detail string
	Have   int
	Want   int
}

// Controller is the leader-side volume controller.
type Controller struct {
	opts Options
	log  *slog.Logger
}

// New builds the controller. Call Run in a goroutine.
func New(opts Options) *Controller {
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Controller{opts: opts, log: opts.Logger}
}

// Run reconciles every tick until the context ends.
func (c *Controller) Run(ctx context.Context) {
	t := time.NewTicker(c.opts.Interval)
	defer t.Stop()
	for {
		if err := c.Reconcile(ctx); err != nil {
			c.log.Warn("volume controller reconcile failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Reconcile converges cluster volume state once. All planning is
// leader-only; non-leaders are inert observers.
func (c *Controller) Reconcile(ctx context.Context) error {
	leader := c.opts.IsLeader != nil && c.opts.IsLeader()
	c.log.Info("controller: reconcile tick", "leader", leader)
	if !leader {
		return nil
	}
	meshed, err := c.meshedNodes(ctx)
	if err != nil {
		return fmt.Errorf("controller: cannot read node membership; skipping this round: %w", err)
	}
	c.processPending(ctx, meshed)
	ids, err := storage.ListVolumeIDs(ctx, c.opts.St)
	if err != nil {
		return err
	}
	if err := c.reconcileBlocks(ctx, meshed); err != nil {
		c.log.Warn("block volume reconcile failed", "err", err)
	}
	if err := c.processVolumeOps(ctx, ids); err != nil {
		c.log.Warn("volume ops failed", "err", err)
	}
	for _, id := range ids {
		c.reconcileVolume(ctx, id, meshed)
	}
	return nil
}

// reconcileVolume runs one volume through its passes; each pass reloads what the last changed.
func (c *Controller) reconcileVolume(ctx context.Context, id string, meshed map[string]bool) {
	spec, err := storage.LoadSpec(ctx, c.opts.St, id)
	if err != nil {
		c.log.Warn("reconcile: load spec failed; skipping volume this round", "vol", id, "err", err)
		return
	}
	status, rev, err := storage.LoadStatus(ctx, c.opts.St, id)
	if err != nil {
		c.log.Warn("reconcile: load status failed; skipping volume this round", "vol", id, "err", err)
		return
	}
	switch status.State {
	case storage.StateDeleting:
		c.finalizeDelete(ctx, id, &spec, &status)
		return
	case storage.StateNeedsManualRecovery:
		return // a human decides; the controller never touches a diverged volume
	}
	c.electPrimary(ctx, id, &status, rev, meshed)
	if status, rev, err = storage.LoadStatus(ctx, c.opts.St, id); err != nil {
		c.log.Warn("reconcile: reload status after election failed", "vol", id, "err", err)
		return
	}
	c.enforceReplication(ctx, id, &spec, &status, rev, meshed)
}

// processVolumeOps consumes operator requests written by
// `expanse ctl volume` (§4.8): delete and move-primary. Each op record
// is one JSON value under /volumes/_ops/<kind>/<volID>.
func (c *Controller) processVolumeOps(ctx context.Context, ids []string) error {
	type op struct {
		Target    string `json:"target"`    // volume NAME (CLI-side lookup)
		To        string `json:"to"`        // move-primary destination node
		SizeBytes uint64 `json:"sizeBytes"` // resize target size
	}
	byName, err := c.volumesByName2(ctx, ids)
	if err != nil {
		return err
	}
	for _, kind := range []string{"delete", "move-primary", "resize"} {
		res, err := c.opts.St.List(ctx, store.Key("/volumes/_ops/"+kind+"/"))
		if err != nil {
			continue
		}
		for _, e := range res {
			volID := strings.TrimPrefix(string(e.Key), "/volumes/_ops/"+kind+"/")
			var o op
			if json.Unmarshal(e.Value, &o) == nil && o.Target != "" {
				if id, ok := c.volIDByName(byName, o.Target); ok {
					volID = id
				}
			}
			status, rev, err := storage.LoadStatus(ctx, c.opts.St, volID)
			if err != nil {
				continue // unknown volume; drop below
			}
			switch kind {
			case "delete":
				if err := c.Delete(ctx, volID); err != nil {
					c.log.Warn("delete op failed", "vol", volID, "err", err)
					continue // retry next tick
				}
			case "move-primary":
				// The destination must already hold a replica; the new
				// primary's runtime runs T11 recovery on bring-up.
				legal := false
				for _, p := range status.Placement {
					if p.NodeID == o.To {
						legal = true
						break
					}
				}
				if !legal {
					c.log.Warn("move-primary refused: target holds no replica", "vol", volID, "to", o.To)
				} else if o.To != status.Primary {
					status.Primary = o.To
					if err := storage.CompareAndSwapStatus(ctx, c.opts.St, volID, rev, status); err != nil {
						continue
					}
					c.log.Info("primary moved", "vol", volID, "to", o.To)
				}
			case "resize":
				// The spec is "immutable-except-size" (storage.Spec's own
				// doc comment) — size is the one field the controller
				// mutates outside creation, grow-only (G6.14): the
				// per-node runtimes converge the LV and DRBD device to it.
				spec, specRev, serr := storage.LoadSpecRev(ctx, c.opts.St, volID)
				if serr != nil {
					break
				}
				if o.SizeBytes <= spec.SizeBytes {
					c.log.Warn("resize op refused: not a grow", "vol", volID, "have", spec.SizeBytes, "want", o.SizeBytes)
					break
				}
				spec.SizeBytes = o.SizeBytes
				if err := storage.CompareAndSwapSpec(ctx, c.opts.St, volID, specRev, spec); err != nil {
					c.log.Warn("resize op CAS failed", "vol", volID, "err", err)
					continue // retry next tick
				}
				c.log.Info("volume resized", "vol", volID, "sizeBytes", o.SizeBytes)
			}
			_ = c.opts.St.Delete(ctx, store.Key(string(e.Key)), 0)
		}
	}
	return nil
}

// volumesByName2 maps volume NAME → ID for the op router.
func (c *Controller) volumesByName2(ctx context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range ids {
		spec, err := storage.LoadSpec(ctx, c.opts.St, id)
		if err != nil {
			continue
		}
		out[spec.Name] = id
	}
	return out, nil
}

func (c *Controller) volIDByName(m map[string]string, name string) (string, bool) {
	id, ok := m[name]
	return id, ok
}

// BlockVolumeName is the cluster volume name for a block's storage
// entry (blocks attach to volumes by name).
func BlockVolumeName(ns, block, storage string) string {
	return fmt.Sprintf("blk-%s-%s-%s", ns, block, storage)
}

// reconcileBlocks drives §4.7 steps 1–2 for blocks with storage:
//  1. every storage entry has a cluster volume (creation flows through
//     the T10 pending-create path → T03 placement);
//  2. the volume's PRIMARY lives on a node hosting the block (the
//     scheduler prefers replica-holding nodes via S3; when the block
//     lands elsewhere, the primary moves there — highest-seq rule).
func (c *Controller) reconcileBlocks(ctx context.Context, meshed map[string]bool) error {
	entries, err := c.opts.St.List(ctx, "/blocks/")
	if err != nil {
		return err
	}
	volByName, err := c.volumesByName(ctx)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasSuffix(string(e.Key), "/status") {
			continue
		}
		var blk pb.Block
		if pbproto.Unmarshal(e.Value, &blk) != nil {
			continue
		}
		ns, name := splitBlockKey2(string(e.Key))
		for _, s := range blk.GetSpec().GetStorage() {
			vname := BlockVolumeName(ns, name, s.GetName())
			if vol, ok := volByName[vname]; ok {
				// §4.7 step 2: primary co-located with the block.
				c.movePrimaryForBlock(vol, blockNodes(&blk), meshed)
				continue
			}
			// §4.7 step 1: create (idempotent pending request).
			size, err := quantity.ParseBytes(s.GetSize())
			if err != nil {
				continue // validation (V12) rejects earlier
			}
			vspec := &pb.VolumeSpec{
				Name:        vname,
				SizeBytes:   uint64(size.N),
				Class:       s.GetClass(),
				Replication: s.GetReplication(),
			}
			raw, err := pbproto.Marshal(vspec)
			if err != nil {
				continue
			}
			if _, err := c.opts.St.Put(ctx, storage.PendingCreateKey(vname), raw); err != nil {
				return err
			}
			c.log.Info("block volume create requested", "block", ns+"/"+name, "vol", vname)
		}
	}
	return nil
}

// movePrimaryForBlock names the node that should be primary among those hosting
// the block: the lowest node ID holding a live replica.
func (c *Controller) movePrimaryForBlock(vol storage.Status, block map[string]bool, meshed map[string]bool) {
	if vol.Primary != "" && block[vol.Primary] {
		return // primary already co-located
	}
	best := ""
	for _, p := range vol.Placement {
		if block[p.NodeID] && meshed[p.NodeID] && (best == "" || p.NodeID < best) {
			best = p.NodeID
		}
	}
	if best == "" {
		return // the block lives nowhere we hold a replica yet
	}
	c.log.Info("primary moves to block host", "vol_primary", vol.Primary, "to", best)
}

// blockNodes maps a block's active placement node IDs.
func blockNodes(blk *pb.Block) map[string]bool {
	out := map[string]bool{}
	if status := blk.GetStatus(); status != nil {
		for _, p := range status.GetPlacements() {
			if p.GetReplicaIndex() >= 0 && p.GetPhase() != pb.Phase_LOST {
				out[p.GetNodeId()] = true
			}
		}
	}
	return out
}

// volumesByName loads every volume's status keyed by NAME.
func (c *Controller) volumesByName(ctx context.Context) (map[string]storage.Status, error) {
	out := map[string]storage.Status{}
	ids, err := storage.ListVolumeIDs(ctx, c.opts.St)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		spec, err := storage.LoadSpec(ctx, c.opts.St, id)
		if err != nil {
			continue
		}
		status, _, err := storage.LoadStatus(ctx, c.opts.St, id)
		if err != nil {
			continue
		}
		out[spec.Name] = status
	}
	return out, nil
}

// splitBlockKey2 splits "/blocks/<ns>/<name>" (bridge's helper, local
// copy to avoid an import cycle).
func splitBlockKey2(k string) (ns, name string) {
	rest := strings.TrimPrefix(k, "/blocks/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i], rest[i+1:]
	}
	return rest, ""
}

func (c *Controller) emitAlert(ev AlertEvent) {
	if c.opts.Alert != nil {
		c.opts.Alert(ev)
	}
	c.log.Warn("volume alert", "vol", ev.VolID, "kind", ev.Kind,
		"have", ev.Have, "want", ev.Want, "detail", ev.Detail)
}
