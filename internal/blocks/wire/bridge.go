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
	expstorage "github.com/expanse/expanse/internal/storage"
	expmount "github.com/expanse/expanse/internal/storage/mount"
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
	// Volume view (T14 §4.7): name → {id, primary node} for every
	// cluster volume. Block storage entries attach to volumes by name.
	vols, err := volumeView(ctx, b.St)
	if err != nil {
		return err
	}
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
			spec, err := replicaSpec(&blk, ns, name, int(p.GetReplicaIndex()), vols)
			if err != nil {
				continue
			}
			key := resourcePrefix + p.GetNodeId() + "/resources/" +
				controller.ReplicaResourceID(ns, name, int(p.GetReplicaIndex()))
			// Canonical reconciler format: "type: <type>" header then
			// the payload (Phase 02 convention).
			want[key] = append([]byte("type: "+systemd.TypeBlockReplica+"\n"), spec...)
			// §4.7: the node serving the volume's PRIMARY mounts the
			// device for this block's unit. One attach
			// resource per (node, volume, mountPath).
			for _, st := range blk.GetSpec().GetStorage() {
				v, ok := vols[st.GetName()]
				if !ok || v.primary != p.GetNodeId() || st.GetMountPath() == "" {
					continue
				}
				ares, _ := json.Marshal(expmount.Resource{
					VolID:      v.id,
					Name:       st.GetName(),
					MountPath:  st.GetMountPath(),
					Filesystem: "ext4",
				})
				akey := resourcePrefix + p.GetNodeId() + "/resources/" + expmount.Type + ":" + v.id
				want[akey] = append([]byte("type: "+expmount.Type+"\n"), ares...)
			}
		}
	}

	// Existing: all block-replica desired keys across nodes.
	existing, err := b.St.List(ctx, resourcePrefix)
	if err != nil {
		return errors.Wrap(err, errors.KindInternal, "bridge.Sync", "list resources")
	}
	for _, e := range existing {
		k := string(e.Key)
		if !isBridgeKey(k) {
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

// isBridgeKey reports whether a desired-state key is one the bridge owns.
func isBridgeKey(k string) bool {
	return strings.Contains(k, "/resources/block-replica:") || strings.Contains(k, "/resources/"+expmount.Type+":")
}

// volumeRef is one cluster volume's placement view.
type volumeRef struct {
	id      string
	primary string
}

// volumeView maps volume NAME → {volume ID, primary node}.
func volumeView(ctx context.Context, st *bStore) (map[string]volumeRef, error) {
	out := map[string]volumeRef{}
	ids, err := expstorage.ListVolumeIDs(ctx, st)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "bridge.volumeView", "list volumes")
	}
	for _, id := range ids {
		spec, err := expstorage.LoadSpec(ctx, st, id)
		if err != nil {
			continue
		}
		status, _, err := expstorage.LoadStatus(ctx, st, id)
		if err != nil {
			continue
		}
		out[spec.Name] = volumeRef{id: id, primary: status.Primary}
	}
	return out, nil
}

// bStore is the store surface the bridge and storage helpers share.
type bStore = raftstore.Store

// replicaSpec builds the JSON block-replica spec for one placement.
// The config travels as a single --config argument (JSON); the runtime
// helper (expanse-block-run) owns the interpretation. Volume storage
// entries bind the volume's host mount into the unit (§4.7 step 6:
// BindPaths=host:declared-mount-path + ReadWritePaths).
func replicaSpec(blk *pb.Block, ns, name string, idx int, vols map[string]volumeRef) ([]byte, error) {
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
	for _, st := range blk.GetSpec().GetStorage() {
		v, ok := vols[st.GetName()]
		if !ok || st.GetMountPath() == "" {
			continue
		}
		host := expmount.HostPath("", v.id)
		spec.BindPaths = append(spec.BindPaths, host+":"+st.GetMountPath())
		spec.VolumeMounts = append(spec.VolumeMounts, host)
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
