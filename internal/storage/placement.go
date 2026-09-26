package storage

import (
	"fmt"
	"sort"

	experrors "github.com/expanse/expanse/internal/errors"
)

// NodeInfo is the placement algorithm's view of one candidate node
// (§4.6 placement bullet). The volume controller (T13) refreshes
// FreeBytes; here it's just data.
type NodeInfo struct {
	ID string
	// Labels is the node's label set, matched against a storage class's
	// NodeSelector (every selector key/value must be present).
	Labels map[string]string
	// FreeBytes is the pool's free space. Nodes with unknown free space
	// (0) sort after known quantities but are still eligible.
	FreeBytes uint64
}

// SelectNodes chooses the node set for a volume's replicas (§4.6):
// spread across distinct nodes, respect the class's nodeSelector, prefer
// nodes with more free pool space, and never place two replicas of the
// same volume on one node. Pure function — no I/O; the controller (T13)
// feeds it current NodeInfos and the volume's existing placements.
//
// existing is the set of node IDs already holding a replica of this
// volume; they are excluded from selection. The result contains exactly
// class.Replication nodes, or a KindResourceExhausted error if fewer
// eligible nodes exist.
func SelectNodes(class StorageClass, nodes []NodeInfo, existing []string) ([]NodeInfo, error) {
	count := class.Replication
	if count <= 0 {
		return nil, experrors.New(experrors.KindInvalid, "storage.SelectNodes",
			fmt.Sprintf("replication must be >= 1, got %d", count))
	}

	exclude := make(map[string]bool, len(existing))
	for _, id := range existing {
		exclude[id] = true
	}

	eligible := make([]NodeInfo, 0, len(nodes))
	for _, n := range nodes {
		if n.ID == "" {
			return nil, experrors.New(experrors.KindInvalid, "storage.SelectNodes", "candidate node missing ID")
		}
		if exclude[n.ID] {
			continue
		}
		if !matchesSelector(n.Labels, class.NodeSelector) {
			continue
		}
		eligible = append(eligible, n)
	}

	if len(eligible) < count {
		return nil, experrors.New(experrors.KindResourceExhausted, "storage.SelectNodes",
			fmt.Sprintf("class %q needs %d replicas on distinct nodes matching selector %v, but only %d eligible nodes exist (excluding %d already holding replicas)",
				class.Name, count, class.NodeSelector, len(eligible), len(existing)))
	}

	// Prefer nodes with more free space; ties break by node ID so the
	// choice is deterministic (and stable across identical cluster
	// states, which keeps controller reconcile convergent).
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].FreeBytes != eligible[j].FreeBytes {
			return eligible[i].FreeBytes > eligible[j].FreeBytes
		}
		return eligible[i].ID < eligible[j].ID
	})

	return eligible[:count], nil
}

// EligibleCount is how many nodes match the class's selector.
func EligibleCount(class StorageClass, nodes []NodeInfo) int {
	n := 0
	for _, node := range nodes {
		if matchesSelector(node.Labels, class.NodeSelector) {
			n++
		}
	}
	return n
}

// matchesSelector reports whether every selector key/value is present in
// the node's labels. An empty selector matches everything.
func matchesSelector(labels map[string]string, selector map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}
