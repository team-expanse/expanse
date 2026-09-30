package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	pbproto "google.golang.org/protobuf/proto"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

// processPending places every queued create request. A request that cannot be placed
// yet stays queued; one that is malformed or names an existing volume is dropped.
func (c *Controller) processPending(ctx context.Context, meshed map[string]bool) {
	if c.opts.Alloc == nil {
		return
	}
	entries, err := c.opts.St.List(ctx, storage.PendingPrefix)
	if err != nil {
		c.log.Warn("cannot list pending volume creates", "err", err)
		return
	}
	for _, e := range entries {
		if c.placeRequest(ctx, e, meshed) {
			_ = c.opts.St.Delete(ctx, e.Key, 0)
		}
	}
}

// placeRequest reports whether the request is finished with and can be dequeued.
func (c *Controller) placeRequest(ctx context.Context, e *store.Entry, meshed map[string]bool) bool {
	var req pb.VolumeSpec
	if pbproto.Unmarshal(e.Value, &req) != nil || req.GetName() == "" {
		return true
	}
	id := volumeID(req.GetName(), e.Revision)
	if taken, err := c.nameTaken(ctx, req.GetName(), id); err != nil || taken {
		if taken {
			c.log.Warn("volume already exists; dropping create request", "vol", req.GetName())
		}
		return taken
	}
	if err := c.place(ctx, &req, id, c.storageNodes(ctx, meshed)); err != nil {
		c.log.Warn("volume placement failed; will retry", "vol", req.GetName(), "err", err)
		c.recordPlacementReason(ctx, req.GetName(), placementReason(err))
		return false
	}
	c.log.Info("volume placed", "vol", req.GetName(), "id", id)
	_ = c.opts.St.Delete(ctx, storage.PlacementReasonKey(req.GetName()), 0)
	return true
}

// placementReason is err's messages alone, without the kind and op prefixes meant for logs.
func placementReason(err error) string {
	var e *experrors.Error
	if !errors.As(err, &e) {
		return err.Error()
	}
	if e.Err == nil {
		return e.Message
	}
	return e.Message + ": " + placementReason(e.Err)
}

// recordPlacementReason stores why a request is still pending, writing only on change
// since placement retries every round.
func (c *Controller) recordPlacementReason(ctx context.Context, name, reason string) {
	key := storage.PlacementReasonKey(name)
	if e, err := c.opts.St.Get(ctx, key); err == nil && string(e.Value) == reason {
		return
	}
	_, _ = c.opts.St.Put(ctx, key, []byte(reason))
}

// volumeID names one create request. It is stable across retries of that request,
// so a crash mid-placement resumes, but never repeats for a recreated name: a
// dead node may still hold the old volume's LV under the old id.
func volumeID(name string, rev store.Revision) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s/%d", name, rev))
	return "vol-" + hex.EncodeToString(sum[:8])
}

// nameTaken reports whether a volume other than id already has the name.
func (c *Controller) nameTaken(ctx context.Context, name, id string) (bool, error) {
	ids, err := storage.ListVolumeIDs(ctx, c.opts.St)
	if err != nil {
		return false, err
	}
	for _, other := range ids {
		if other == id {
			continue
		}
		spec, err := storage.LoadSpec(ctx, c.opts.St, other)
		if experrors.Is(err, experrors.KindNotFound) {
			continue
		}
		if err != nil {
			return false, err // an unread volume may hold the name
		}
		if spec.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// place allocates the DRBD identity, then writes status and finally the spec: the
// spec is what nodes discover volumes by, so it commits the placement.
func (c *Controller) place(ctx context.Context, req *pb.VolumeSpec, id string, nodes []storage.NodeInfo) error {
	class, err := c.resolveClass(ctx, req)
	if err != nil {
		return err
	}
	al, err := c.opts.Alloc.Allocate(ctx, id)
	if err != nil {
		return err
	}
	hosts, err := replicaHosts(al.NodeIDs, req, class, nodes)
	if err != nil {
		return err
	}
	for _, h := range hosts {
		if _, err := c.opts.Alloc.AssignNodeID(ctx, id, h); err != nil {
			return err
		}
	}
	status := storage.Status{State: storage.StateCreating, Primary: hosts[0]}
	for _, h := range hosts {
		status.Placement = append(status.Placement, storage.Replica{NodeID: h})
	}
	if err := storage.SaveStatus(ctx, c.opts.St, id, status); err != nil {
		return err
	}
	return storage.SaveSpec(ctx, c.opts.St, storage.Spec{
		ID: id, Name: req.GetName(), Namespace: "default", SizeBytes: req.GetSizeBytes(),
		Class: req.GetClass(), Replication: class.Replication,
	})
}

// resolveClass is the request's storage class from the cluster catalog, with an
// explicit replication overriding the class's own. The spec stores this as the target.
func (c *Controller) resolveClass(ctx context.Context, req *pb.VolumeSpec) (storage.StorageClass, error) {
	var raw []byte
	if e, err := c.opts.St.Get(ctx, storage.StorageClassesKey); err == nil {
		raw = e.Value
	}
	classes, err := storage.ParseStorageClasses(raw)
	if err != nil {
		return storage.StorageClass{}, err
	}
	name := req.GetClass()
	if name == "" {
		name = "default"
	}
	class, ok := storage.GetClass(classes, name)
	switch {
	case !ok && name == "default":
		class = storage.DefaultStorageClass()
	case !ok:
		return storage.StorageClass{}, experrors.New(experrors.KindInvalid, "controller.place", "unknown storage class "+name)
	}
	if n := int(req.GetReplication()); n > 0 {
		class.Replication = n
	}
	return class, nil
}

// replicaHosts keeps the hosts a crashed earlier attempt already gave node-ids, and
// otherwise selects them: as many as the class's target when replication is explicit,
// else up to the target on the nodes there are (rebuild grows the rest later).
func replicaHosts(assigned map[string]int, req *pb.VolumeSpec, class storage.StorageClass, nodes []storage.NodeInfo) ([]string, error) {
	if len(assigned) > 0 {
		hosts := make([]string, 0, len(assigned))
		for h := range assigned {
			hosts = append(hosts, h)
		}
		slices.SortFunc(hosts, func(a, b string) int { return assigned[a] - assigned[b] })
		return hosts, nil
	}
	eligible := storage.EligibleCount(class, nodes)
	if req.GetReplication() > 0 && eligible < class.Replication {
		return nil, experrors.New(experrors.KindResourceExhausted, "controller.place",
			fmt.Sprintf("needs %d nodes, %d eligible", class.Replication, eligible))
	}
	class.Replication = max(1, min(class.Replication, eligible))
	// A per-replica volume (D3) must land on its owning block replica's
	// own node, not wherever SelectNodes' capacity ranking would
	// otherwise pick — that ranking has no idea a block replica exists
	// at all, let alone which node it's on.
	if pin := req.GetPreferredNode(); pin != "" && class.Replication == 1 {
		for _, n := range nodes {
			if n.ID == pin {
				return []string{pin}, nil
			}
		}
		return nil, experrors.New(experrors.KindResourceExhausted, "controller.place",
			"preferred node "+pin+" is not a current placement candidate")
	}
	chosen, err := storage.SelectNodes(class, nodes, nil)
	if err != nil {
		return nil, experrors.Wrap(err, experrors.KindOf(err), "controller.place", "select replica nodes")
	}
	hosts := make([]string, len(chosen))
	for i, n := range chosen {
		hosts[i] = n.ID
	}
	return hosts, nil
}
