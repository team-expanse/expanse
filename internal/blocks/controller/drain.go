package controller

import (
	"context"
	"maps"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

// NodeReplica names one replica of one block.
type NodeReplica struct {
	Block string // namespace/name
	Index int32
}

// blockStatuses maps each block ("namespace/name") to its observed status.
func blockStatuses(ctx context.Context, st store.Store) (map[string]*pb.BlockStatus, error) {
	entries, err := st.List(ctx, "/blocks/")
	if err != nil {
		return nil, errors.Wrap(err, errors.KindUnavailable, "controller.blockStatuses", "list blocks")
	}
	out := map[string]*pb.BlockStatus{}
	for _, e := range entries {
		name, ok := strings.CutSuffix(strings.TrimPrefix(string(e.Key), "/blocks/"), StatusSuffix)
		if !ok || strings.Count(name, "/") != 1 {
			continue
		}
		var s pb.BlockStatus
		if proto.Unmarshal(e.Value, &s) == nil {
			out[name] = &s
		}
	}
	return out, nil
}

// NodeReplicas lists the live replicas placed on node, in block order.
func NodeReplicas(ctx context.Context, st store.Store, node string) ([]NodeReplica, error) {
	statuses, err := blockStatuses(ctx, st)
	if err != nil {
		return nil, err
	}
	var out []NodeReplica
	for _, name := range slices.Sorted(maps.Keys(statuses)) {
		for _, p := range statuses[name].GetPlacements() {
			if p.GetNodeId() == node && p.GetReplicaIndex() >= 0 {
				out = append(out, NodeReplica{name, p.GetReplicaIndex()})
			}
		}
	}
	return out, nil
}

// DrainProgress splits the replicas that were on node into those still moving and those held
// there (with why). A replica running elsewhere, or no longer wanted, has moved.
func DrainProgress(ctx context.Context, st store.Store, node string, was []NodeReplica) (moving []NodeReplica, held map[NodeReplica]string, err error) {
	statuses, err := blockStatuses(ctx, st)
	if err != nil {
		return nil, nil, err
	}
	held = map[NodeReplica]string{}
	for _, r := range was {
		var cur *pb.PlacementStatus
		for _, p := range statuses[r.Block].GetPlacements() {
			if p.GetReplicaIndex() == r.Index && (cur == nil || p.GetNodeId() == node) {
				cur = p
			}
		}
		switch {
		case cur == nil:
		case cur.GetNodeId() == node && cur.GetHeld() != "":
			held[r] = cur.GetHeld()
		case cur.GetNodeId() == node || cur.GetPhase() != pb.Phase_RUNNING:
			moving = append(moving, r)
		}
	}
	return moving, held, nil
}
