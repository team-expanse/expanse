package volume

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
)

// opResolve carries the operator's choice of survivor for a diverged volume, one
// request per replica under /volumes/_ops/resolve/<volID>/<node>.
const opResolve = "resolve"

type resolveOp struct {
	Survivor string `json:"survivor"`
}

func resolveKey(id, node string) store.Key { return opKey(opResolve, id+"/"+node) }

func resolvePrefix(id string) store.Key { return store.Key(string(opKey(opResolve, id)) + "/") }

// runResolve carries out this node's request, if any. A discarding replica acts at
// once; the survivor waits until no other replica has a request left, so nobody
// reconnects before the others have given up their data. status is only read: what
// the survivor writes is seen by the next pass, and this one still treats the volume as diverged.
func (n *Node) runResolve(ctx context.Context, d Desired, status storage.Status) error {
	key := resolveKey(d.Name, n.Self)
	entry, err := n.St.Get(ctx, key)
	if err != nil {
		return ignoreMissing(err)
	}
	var op resolveOp
	if err := json.Unmarshal(entry.Value, &op); err != nil {
		n.log().Warn("dropping an unreadable request", "kind", opResolve, "vol", d.Name, "err", err)
		return n.St.Delete(ctx, key, 0)
	}
	survivor := op.Survivor == n.Self
	if !n.resolvable(d.Name, op, status, survivor) {
		n.log().Warn("dropping a request that no longer applies", "vol", d.Name, "survivor", op.Survivor)
		return n.St.Delete(ctx, key, 0)
	}
	n.stopLeading(d.Name)
	if survivor {
		return n.keepData(ctx, d, key)
	}
	return n.discardData(ctx, d, key)
}

// resolvable reports whether the request still applies: the survivor holds a
// replica (else every replica would discard), and the volume is diverged, which is
// how a request answered by another node's earlier run is recognised.
func (n *Node) resolvable(id string, op resolveOp, status storage.Status, survivor bool) bool {
	if !slices.ContainsFunc(status.Placement, func(r storage.Replica) bool { return r.NodeID == op.Survivor }) {
		return false
	}
	if status.State == storage.StateNeedsManualRecovery {
		return true
	}
	marked, err := n.Splits.Marked(id)
	return !survivor && err == nil && marked
}

func (n *Node) discardData(ctx context.Context, d Desired, key store.Key) error {
	if err := n.RT.Rejoin(ctx, d, true); err != nil {
		return err
	}
	if err := n.Splits.Clear(d.Name); err != nil {
		return err
	}
	return n.St.Delete(ctx, key, 0)
}

func (n *Node) keepData(ctx context.Context, d Desired, key store.Key) error {
	others, err := n.St.List(ctx, resolvePrefix(d.Name))
	if err != nil {
		return err
	}
	if slices.ContainsFunc(others, func(e *store.Entry) bool { return e.Key != key }) {
		return nil // another replica has not discarded yet
	}
	if err := n.RT.Rejoin(ctx, d, false); err != nil {
		return err
	}
	if err := n.Splits.Clear(d.Name); err != nil {
		return err
	}
	if err := n.returnToService(ctx, d.Name); err != nil {
		return err
	}
	return n.St.Delete(ctx, key, 0)
}

// returnToService makes this node the primary of a volume out of manual recovery,
// Degraded until the controller sees every replica healthy again.
func (n *Node) returnToService(ctx context.Context, id string) error {
	return n.updateStatus(ctx, id, func(st *storage.Status) bool {
		if st.State != storage.StateNeedsManualRecovery {
			return false
		}
		st.State, st.Primary = storage.StateDegraded, n.Self
		return true
	})
}
