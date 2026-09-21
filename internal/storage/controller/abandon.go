package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
)

// abandonDownNodes lets a deletion go ahead without the nodes that are down: it marks
// each one to remove its replica when it returns, then drops its placement row, which
// is the proof a delete otherwise waits for. Live nodes still have to report.
func (c *Controller) abandonDownNodes(ctx context.Context, volID string, meshed map[string]bool) error {
	status, rev, err := storage.LoadStatus(ctx, c.opts.St, volID)
	if err != nil {
		return err
	}
	var down []string
	for _, r := range status.Placement {
		if !meshed[r.NodeID] {
			down = append(down, r.NodeID)
		}
	}
	for _, node := range down {
		if err := storage.PutGone(ctx, c.opts.St, node, volID); err != nil {
			return err
		}
	}
	if len(down) == 0 {
		return nil
	}
	status.Placement = slices.DeleteFunc(status.Placement, func(r storage.Replica) bool { return slices.Contains(down, r.NodeID) })
	c.log.Warn("delete forced past nodes that are down", "vol", volID, "nodes", down)
	return storage.CompareAndSwapStatus(ctx, c.opts.St, volID, rev, status)
}

// retireLost gives up a down node's replica at the operator's word, without waiting
// out LostAfter or for a spare. A refused request is logged and dropped by the caller;
// a failure is returned so the request is retried.
func (c *Controller) retireLost(ctx context.Context, volID, host string, meshed map[string]bool) error {
	status, _, err := storage.LoadStatus(ctx, c.opts.St, volID)
	if err != nil {
		return err
	}
	if why := retireRefusal(status, host, meshed); why != "" {
		c.log.Warn("retire refused", "vol", volID, "node", host, "why", why)
		return nil
	}
	if _, err := c.opts.Alloc.RetireNode(ctx, volID, host); err != nil && experrors.KindOf(err) != experrors.KindNotFound {
		return err // NotFound: an earlier attempt already retired the id
	}
	c.dropNodeRequests(ctx, volID, host)
	c.alignPlacement(ctx, volID)
	if status, _, err = storage.LoadStatus(ctx, c.opts.St, volID); err != nil {
		return err
	}
	if slices.ContainsFunc(status.Placement, func(r storage.Replica) bool { return r.NodeID == host }) {
		return fmt.Errorf("the placement still lists %s", host)
	}
	c.log.Warn("replica given up on at the operator's request", "vol", volID, "node", host)
	return nil
}

func retireRefusal(status storage.Status, host string, meshed map[string]bool) string {
	switch {
	case status.State == storage.StateDeleting:
		return "the volume is being deleted"
	case !slices.ContainsFunc(status.Placement, func(r storage.Replica) bool { return r.NodeID == host }):
		return "the node holds no replica of it"
	case meshed[host]:
		return "the node is alive"
	case !slices.ContainsFunc(status.Placement, func(r storage.Replica) bool { return meshed[r.NodeID] }):
		return "no other replica is reachable, so nothing would keep the data"
	}
	return ""
}

// dropNodeRequests removes the per-node requests addressed to a node that has left the
// volume; a resolve survivor would otherwise wait on its discard for ever.
func (c *Controller) dropNodeRequests(ctx context.Context, volID, host string) {
	var errs []error
	for _, kind := range []string{"resolve", "resync"} {
		errs = append(errs, c.opts.St.Delete(ctx, store.Key(opsPrefix+kind+"/"+volID+"/"+host), 0))
	}
	if err := errors.Join(errs...); err != nil {
		c.log.Warn("cannot drop the requests of a retired node", "vol", volID, "node", host, "err", err)
	}
}
