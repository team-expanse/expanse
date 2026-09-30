package controller

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/cluster/lease"
	"github.com/expanse/expanse/internal/cluster/nodelc"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
)

// leaseSkew is allowed for clock differences: expiry is judged on the local clock,
// the grant was made on the holder's.
const leaseSkew = 15 * time.Second

// nodeRecord reads a node's cluster record; ok is false when it has none yet.
func (c *Controller) nodeRecord(ctx context.Context, id string) (rec join.NodeRecord, ok bool) {
	e, err := c.opts.St.Get(ctx, store.Key("/nodes/"+id))
	if err != nil || json.Unmarshal(e.Value, &rec) != nil {
		return join.NodeRecord{}, false
	}
	return rec, true
}

// isDown reports that the failure monitor has marked the node unreachable or failed.
func isDown(rec join.NodeRecord) bool {
	return rec.State == nodelc.StateUnreachable || rec.State == nodelc.StateFailed
}

// meshedNodes maps every node with a mesh record to whether it is alive. The record
// outlives a hard-killed node, so it says nothing by itself. Two signals say a node
// is gone: its liveness lease expired (a crash), or the failure monitor marked its
// record unreachable or failed (which also covers a clean stop, since a stopping
// agent releases its lease). A node with neither signal is alive; that includes one
// with no lease at all (single-bolt clusters, tests).
func (c *Controller) meshedNodes(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	entries, err := c.opts.St.List(ctx, "/nodes/")
	if err != nil {
		return nil, err
	}
	leases := lease.NewManager(c.opts.St, "storage-controller")
	now := time.Now()
	for _, e := range entries {
		if !strings.HasSuffix(string(e.Key), "/network.wgPublicKey") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(string(e.Key), "/nodes/"), "/network.wgPublicKey")
		alive := true
		if l, ok, err := leases.Inspect(ctx, "node-"+id); err == nil && ok && now.After(l.ExpiresAt.Add(leaseSkew)) {
			alive = false
		}
		if rec, ok := c.nodeRecord(ctx, id); ok && isDown(rec) {
			alive = false
		}
		out[id] = alive
	}
	return out, nil
}

// storageNodes are the live nodes that can take a new replica: not witnesses (no
// capacity) and not cordoned.
func (c *Controller) storageNodes(ctx context.Context, meshed map[string]bool) []storage.NodeInfo {
	held := c.replicaCounts(ctx)
	var nodes []storage.NodeInfo
	for id, alive := range meshed {
		rec, _ := c.nodeRecord(ctx, id)
		if alive && rec.Role != "witness" && !rec.Cordoned {
			nodes = append(nodes, storage.NodeInfo{ID: id, Replicas: held[id]})
		}
	}
	slices.SortFunc(nodes, func(a, b storage.NodeInfo) int { return strings.Compare(a.ID, b.ID) })
	return nodes
}

// replicaCounts is how many volume replicas each node holds, so placement can spread new ones.
func (c *Controller) replicaCounts(ctx context.Context) map[string]int {
	held := map[string]int{}
	ids, err := storage.ListVolumeIDs(ctx, c.opts.St)
	if err != nil {
		return held
	}
	for _, id := range ids {
		if status, _, err := storage.LoadStatus(ctx, c.opts.St, id); err == nil {
			for _, r := range status.Placement {
				held[r.NodeID]++
			}
		}
	}
	return held
}
