package volume

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
)

// Operator requests that only the volume's primary node can carry out. The CLI
// writes one JSON value per volume under /volumes/_ops/<kind>/<volID>.
const (
	opSnapshot = "snapshot"
	opRestore  = "restore"
	opVerify   = "verify"
)

type snapshotOp struct {
	Name string `json:"name"`
}

func opKey(kind, id string) store.Key { return store.Key("/volumes/_ops/" + kind + "/" + id) }

// runOps carries out the snapshot, restore and verify requests queued for a volume this
// node leads. A refused request is dropped with a log line; one that failed for a
// passing reason stays queued and is retried on the next sync.
func (n *Node) runOps(ctx context.Context, d Desired) error {
	return errors.Join(
		n.runOp(ctx, opSnapshot, d.Name, func(name string) error { return n.takeSnapshot(ctx, d, name) }),
		n.runOp(ctx, opRestore, d.Name, func(name string) error { return n.restoreSnapshot(ctx, d, name) }),
		n.runBareOp(ctx, opKey(opVerify, d.Name), opVerify, d.Name, func() error { return n.RT.Verify(ctx, d.Name) }),
	)
}

func (n *Node) runOp(ctx context.Context, kind, id string, do func(name string) error) error {
	entry, err := n.St.Get(ctx, opKey(kind, id))
	if err != nil {
		return ignoreMissing(err) // missing: nothing queued
	}
	var op snapshotOp
	if err := json.Unmarshal(entry.Value, &op); err != nil {
		n.log().Warn("dropping an unreadable request", "kind", kind, "vol", id, "err", err)
		return n.St.Delete(ctx, opKey(kind, id), 0)
	}
	return n.settle(ctx, opKey(kind, id), kind, id, do(op.Name))
}

// runBareOp is runOp for a request that carries no argument.
func (n *Node) runBareOp(ctx context.Context, key store.Key, kind, id string, do func() error) error {
	if _, err := n.St.Get(ctx, key); err != nil {
		return ignoreMissing(err) // missing: nothing queued
	}
	return n.settle(ctx, key, kind, id, do())
}

// settle finishes a request given the outcome of carrying it out: a refusal is
// logged and drops it, any other failure leaves it queued.
func (n *Node) settle(ctx context.Context, key store.Key, kind, id string, err error) error {
	switch experrors.KindOf(err) {
	case "":
	case experrors.KindInvalid, experrors.KindNotFound:
		n.log().Warn("request refused", "kind", kind, "vol", id, "err", err)
	default:
		return err
	}
	return n.St.Delete(ctx, key, 0)
}

// takeSnapshot snapshots this node's replica and records it, so the cluster knows
// where the snapshot lives. A name another node already holds is refused.
func (n *Node) takeSnapshot(ctx context.Context, d Desired, name string) error {
	if err := storage.ValidSnapshotName(name); err != nil {
		return err
	}
	held, err := n.holder(ctx, d.Name, name)
	if err != nil {
		return err
	}
	if held != "" && held != n.Self {
		return experrors.New(experrors.KindInvalid, "volume.takeSnapshot", "snapshot "+name+" already exists on "+held)
	}
	if err := n.RT.Snapshot(ctx, d.Name, name); err != nil {
		return err
	}
	return storage.PutSnapshot(ctx, n.St, d.Name, storage.SnapshotRecord{Name: name, Node: n.Self, CreatedAt: time.Now().UTC()})
}

// restoreSnapshot rolls the volume back to a snapshot this node holds.
func (n *Node) restoreSnapshot(ctx context.Context, d Desired, name string) error {
	held, err := n.holder(ctx, d.Name, name)
	switch {
	case err != nil:
		return err
	case held == "":
		return experrors.New(experrors.KindNotFound, "volume.restoreSnapshot", "no snapshot "+name)
	case held != n.Self:
		return experrors.New(experrors.KindInvalid, "volume.restoreSnapshot", "snapshot "+name+" is held by "+held+"; make it the primary first")
	}
	return n.RT.Restore(ctx, d, name)
}

// holder names the node holding a snapshot, or "" when none is recorded.
func (n *Node) holder(ctx context.Context, id, name string) (string, error) {
	recs, err := storage.ListSnapshotRecords(ctx, n.St, id)
	if err != nil {
		return "", err
	}
	for _, r := range recs {
		if r.Name == name {
			return r.Node, nil
		}
	}
	return "", nil
}
