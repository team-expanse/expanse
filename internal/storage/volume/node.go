package volume

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"time"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/storage/drbd"
	"github.com/expanse/expanse/internal/store"
)

// Converger is the node-local half of a volume; *Runtime implements it.
type Converger interface {
	Reconcile(ctx context.Context, d Desired) (Result, error)
	Remove(ctx context.Context, name string) error
	Present(ctx context.Context, name string) (bool, error)
	Snapshot(ctx context.Context, name, snap string) error
	Restore(ctx context.Context, d Desired, snap string) error
}

// Leader keeps one volume primary on this node until ctx ends and demotes it on
// the way out; it is Promoter.Lead with the lease manager and TTL bound.
type Leader func(ctx context.Context, res string, opt HoldOptions) error

// LeaseName is the lease that makes a node the volume's primary.
func LeaseName(res string) string { return "primary-" + res }

// Node converges this machine's share of every volume in the cluster store: it
// reconciles each placed replica, runs the promotion loop for the volumes this
// node is elected primary of, tears down the ones it no longer holds, and
// reports what DRBD sees back into the volume's placement row.
type Node struct {
	Self  string
	St    store.Store
	Alloc *drbd.Allocator
	RT    Converger
	DRBD  drbd.DRBD
	Lead  Leader
	// Splits holds the kernel's split-brain reports for this node's replicas.
	Splits SplitBrainMarks
	// Addr resolves a member host to its mesh address.
	Addr func(host string) (netip.Addr, error)
	Thin bool
	Log  *slog.Logger

	leading map[string]*leadership
}

type leadership struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (n *Node) log() *slog.Logger {
	if n.Log != nil {
		return n.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Run syncs every interval until ctx ends, then stops every leader.
func (n *Node) Run(ctx context.Context, interval time.Duration) {
	defer n.Stop()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := n.Sync(ctx); err != nil && ctx.Err() == nil {
			n.log().Warn("volume sync incomplete", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sync makes one pass over every volume. A volume that fails does not hold up the
// others; the errors are joined. Leaders started here live until stopped, not until
// this call returns, so ctx should be the long-lived one.
func (n *Node) Sync(ctx context.Context) error {
	ids, err := storage.ListVolumeIDs(ctx, n.St)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := n.syncVolume(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("volume %s: %w", id, err))
		}
	}
	for id := range n.leading {
		if !slices.Contains(ids, id) {
			n.stopLeading(id)
		}
	}
	return errors.Join(errs...)
}

func (n *Node) syncVolume(ctx context.Context, id string) error {
	spec, err := storage.LoadSpec(ctx, n.St, id)
	if err != nil {
		return ignoreMissing(err)
	}
	status, _, err := storage.LoadStatus(ctx, n.St, id)
	if err != nil {
		return ignoreMissing(err)
	}
	placed := slices.ContainsFunc(status.Placement, func(r storage.Replica) bool { return r.NodeID == n.Self })
	if !placed || status.State == storage.StateDeleting {
		return n.release(ctx, id, placed)
	}
	al, err := n.Alloc.Get(ctx, id)
	if err != nil {
		return ignoreMissing(err) // the leader has not allocated yet
	}
	d, err := n.desired(al, spec)
	if err != nil {
		return err
	}
	diverged, err := n.diverged(ctx, id, &status)
	if err != nil {
		return err
	}
	if diverged {
		// An adjust would reconnect the dropped resource and repeat the split-brain.
		n.stopLeading(id)
		return n.publish(ctx, d)
	}
	lead := status.Primary == n.Self
	if !lead {
		n.stopLeading(id)
	}
	res, err := n.RT.Reconcile(ctx, d)
	if err != nil {
		return err
	}
	errs := n.acknowledge(ctx, id, res.Forgot)
	if lead {
		n.startLeading(ctx, al)
		errs = append(errs, n.runOps(ctx, d))
	}
	return errors.Join(append(errs, n.publish(ctx, d))...)
}

// diverged reports whether the volume needs manual recovery, moving it there when
// the kernel has reported a split-brain. Nothing here resolves it.
func (n *Node) diverged(ctx context.Context, id string, status *storage.Status) (bool, error) {
	if status.State == storage.StateNeedsManualRecovery {
		return true, nil
	}
	marked, err := n.Splits.Marked(id)
	if err != nil || !marked {
		return false, err
	}
	status.State = storage.StateNeedsManualRecovery
	return true, n.updateStatus(ctx, id, func(st *storage.Status) bool {
		if st.State == storage.StateDeleting || st.State == storage.StateNeedsManualRecovery {
			return false
		}
		st.State = storage.StateNeedsManualRecovery
		return true
	})
}

// ignoreMissing treats a record that is not there yet (creation is two writes) as no work.
func ignoreMissing(err error) error {
	if experrors.KindOf(err) == experrors.KindNotFound {
		return nil
	}
	return err
}

func (n *Node) desired(al drbd.Allocation, spec storage.Spec) (Desired, error) {
	addrs := make(map[string]netip.Addr, len(al.NodeIDs))
	for host := range al.NodeIDs {
		addr, err := n.Addr(host)
		if err != nil {
			return Desired{}, fmt.Errorf("address of %s: %w", host, err)
		}
		addrs[host] = addr
	}
	return FromAllocation(al, n.Self, spec.SizeBytes, n.Thin, addrs)
}

func (n *Node) acknowledge(ctx context.Context, id string, forgot []int) []error {
	var errs []error
	for _, nodeID := range forgot {
		if err := n.Alloc.AckForgotten(ctx, id, nodeID, n.Self); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// release takes a volume this node should not hold off the node. A deleted volume
// then drops this node's row, which is how the controller learns the node is done.
func (n *Node) release(ctx context.Context, id string, placed bool) error {
	n.stopLeading(id)
	present, err := n.RT.Present(ctx, id)
	if err != nil {
		return err
	}
	if present {
		if err := n.RT.Remove(ctx, id); err != nil {
			return err
		}
		if err := n.Splits.Clear(id); err != nil {
			return err
		}
	}
	if !placed {
		return nil
	}
	return n.updateStatus(ctx, id, func(st *storage.Status) bool {
		before := len(st.Placement)
		st.Placement = slices.DeleteFunc(st.Placement, func(r storage.Replica) bool { return r.NodeID == n.Self })
		return len(st.Placement) != before
	})
}

func (n *Node) startLeading(ctx context.Context, al drbd.Allocation) {
	if n.leading[al.Name] != nil {
		return
	}
	lctx, cancel := context.WithCancel(ctx)
	l := &leadership{cancel: cancel, done: make(chan struct{})}
	if n.leading == nil {
		n.leading = map[string]*leadership{}
	}
	n.leading[al.Name] = l
	opt := HoldOptions{Initial: !al.Initialized, OnPrimary: func() { n.markInitialized(al.Name) }}
	go func() {
		defer close(l.done)
		if err := n.Lead(lctx, al.Name, opt); err != nil {
			n.log().Error("leading ended in error", "vol", al.Name, "err", err)
		}
	}()
}

func (n *Node) markInitialized(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := n.Alloc.MarkInitialized(ctx, id); err != nil {
		n.log().Warn("could not record the first promotion", "vol", id, "err", err)
	}
}

// stopLeading ends a volume's leader and waits for it to demote.
func (n *Node) stopLeading(id string) {
	l := n.leading[id]
	if l == nil {
		return
	}
	l.cancel()
	<-l.done
	delete(n.leading, id)
}

// Stop ends every leader, each demoting its volume before this returns.
func (n *Node) Stop() {
	for id := range n.leading {
		n.stopLeading(id)
	}
}

// publish reports this node's own replica into the placement row, writing only
// when the role or health changed.
func (n *Node) publish(ctx context.Context, d Desired) error {
	st, err := n.DRBD.Status(ctx, d.Name)
	if err != nil {
		return ignoreMissing(err) // the resource is not up yet
	}
	obs := Observe(st, n.Self, d.Members)
	i := slices.IndexFunc(obs.Replicas, func(r Replica) bool { return r.NodeID == n.Self })
	if i < 0 {
		return nil
	}
	mine := obs.Replicas[i]
	return n.updateStatus(ctx, d.Name, func(s *storage.Status) bool {
		j := slices.IndexFunc(s.Placement, func(r storage.Replica) bool { return r.NodeID == n.Self })
		if j < 0 || (s.Placement[j].Role == mine.Role && s.Placement[j].Healthy == mine.Healthy) {
			return false
		}
		s.Placement[j].Role, s.Placement[j].Healthy, s.Placement[j].LastSeen = mine.Role, mine.Healthy, time.Now().UTC()
		return true
	})
}

const statusAttempts = 4

// updateStatus applies fn to the volume's status and writes it back if fn reports a
// change, retrying when the controller wrote in between.
func (n *Node) updateStatus(ctx context.Context, id string, fn func(*storage.Status) bool) error {
	var err error
	for range statusAttempts {
		var (
			st  storage.Status
			rev store.Revision
		)
		if st, rev, err = storage.LoadStatus(ctx, n.St, id); err != nil {
			return ignoreMissing(err)
		}
		if !fn(&st) {
			return nil
		}
		if err = storage.CompareAndSwapStatus(ctx, n.St, id, rev, st); err == nil {
			return nil
		}
	}
	return err
}
