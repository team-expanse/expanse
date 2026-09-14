package wire

// Desired-state bridge (T20.5b): the leader-side loop that turns
// persisted placements into per-node desired state. For every block
// status placement (index >= 0, not LOST) it ensures
//
//	/node/<node>/resources/block-replica:<ns>/<name>/<idx>
//
// holds the JSON block-replica Spec the target agent's reconciler
// converges into a systemd unit (internal/blocks/runtime/systemd).
// Retired/deleted placements get their resource keys removed.
//
// The resource id matches controller.ReplicaResourceID; the agent
// publishes per-resource status under
// /node/<node>/status/resources/<id>, which controller.RuntimePass
// reads to promote placements to RUNNING.

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/blocks/controller"
	"github.com/expanse/expanse/internal/blocks/runtime/systemd"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// DefaultBridgeInterval is the bridge's reconcile ticker; watches are
// the fast path.
const DefaultBridgeInterval = 10 * time.Second

// resourcePrefix is the per-node desired-state prefix (matches
// reconcile.DesiredPrefix layout: /node/<id>/resources/).
const resourcePrefix = "/node/"

// Bridge materializes placements as desired state on their nodes.
type Bridge struct {
	St *raftstore.Store
	// Interval between full syncs; DefaultBridgeInterval when zero.
	Interval time.Duration
}

// Run syncs until ctx is done: on every tick and on any /blocks/ write.
func (b *Bridge) Run(ctx context.Context) {
	ival := b.Interval
	if ival <= 0 {
		ival = DefaultBridgeInterval
	}
	t := time.NewTicker(ival)
	defer t.Stop()
	wake, cancel, err := b.watch(ctx)
	if err != nil {
		// No watch: the ticker alone still converges (slower).
		wake, cancel = nil, func() {}
	}
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-wake:
		}
		_ = b.Sync(ctx) //nolint: next tick retries; never fatal
	}
}

// watch delivers store events on /blocks/ into a wake channel.
func (b *Bridge) watch(ctx context.Context) (<-chan struct{}, func(), error) {
	wake := make(chan struct{}, 1)
	ch, err := b.St.Watch(ctx, "/blocks/", 0)
	if err != nil {
		return nil, nil, errors.Wrap(err, errors.KindInternal, "bridge.Run", "watch blocks")
	}
	go func() {
		for range ch {
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}()
	return wake, func() {}, nil
}

// Sync converges the desired-state keys with current placements.
func (b *Bridge) Sync(ctx context.Context) error {
	// Wanted: resource key -> spec JSON.
	want := map[string][]byte{}
	entries, err := b.St.List(ctx, "/blocks/")
	if err != nil {
		return errors.Wrap(err, errors.KindInternal, "bridge.Sync", "list blocks")
	}
	for _, e := range entries {
		if strings.HasSuffix(string(e.Key), "/status") {
			continue
		}
		var status pb.BlockStatus
		se, err := b.St.Get(ctx, store.Key(string(e.Key)+"/status"))
		if err != nil {
			continue // nothing placed yet
		}
		if err := proto.Unmarshal(se.Value, &status); err != nil {
			continue
		}
		var blk pb.Block
		if err := proto.Unmarshal(e.Value, &blk); err != nil {
			continue
		}
		ns, name := splitBlockKey(string(e.Key))
		for _, p := range status.GetPlacements() {
			if p.GetReplicaIndex() < 0 || p.GetNodeId() == "" || p.GetPhase() == pb.Phase_LOST {
				continue
			}
			spec, err := replicaSpec(&blk, ns, name, int(p.GetReplicaIndex()))
			if err != nil {
				continue
			}
			key := resourcePrefix + p.GetNodeId() + "/resources/" +
				controller.ReplicaResourceID(ns, name, int(p.GetReplicaIndex()))
			want[key] = spec
		}
	}

	// Existing: all block-replica desired keys across nodes.
	existing, err := b.St.List(ctx, resourcePrefix)
	if err != nil {
		return errors.Wrap(err, errors.KindInternal, "bridge.Sync", "list resources")
	}
	for _, e := range existing {
		k := string(e.Key)
		if !strings.Contains(k, "/resources/block-replica:") {
			continue
		}
		if v, ok := want[k]; ok && string(v) == string(e.Value) {
			delete(want, k) // in sync
			continue
		}
		if _, ok := want[k]; ok {
			continue // changed: put below
		}
		if err := b.St.Delete(ctx, store.Key(k), 0); err != nil && !errors.Is(err, errors.KindNotFound) {
			return errors.Wrap(err, errors.KindUnavailable, "bridge.Sync", "delete "+k)
		}
	}
	for k, v := range want {
		if _, err := b.St.Put(ctx, store.Key(k), v); err != nil {
			return errors.Wrap(err, errors.KindUnavailable, "bridge.Sync", "put "+k)
		}
	}
	return nil
}

// replicaSpec builds the JSON block-replica spec for one placement.
// The config travels as a single --config argument (JSON); the runtime
// helper (expanse-block-run) owns the interpretation.
func replicaSpec(blk *pb.Block, ns, name string, idx int) ([]byte, error) {
	spec := systemd.Spec{
		Namespace: ns,
		Name:      name,
		Index:     idx,
		Type:      blk.GetSpec().GetType(),
	}
	if cfg := blk.GetSpec().GetConfig(); cfg != nil {
		if raw, err := cfg.MarshalJSON(); err == nil {
			spec.Args = []string{"--config", string(raw)}
		}
	}
	return json.Marshal(spec)
}

func splitBlockKey(k string) (ns, name string) {
	rest := strings.TrimPrefix(k, "/blocks/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i], rest[i+1:]
	}
	return rest, ""
}
