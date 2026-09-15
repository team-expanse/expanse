package mesh

import (
	"context"
	"fmt"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// IndexKey returns the store key recording that nodeID holds overlay
// index idx: /network/nodeIndexes/<idx> → nodeID.
func IndexKey(idx int) store.Key {
	return store.Key(fmt.Sprintf("/network/nodeIndexes/%d", idx))
}

// ClaimIndex assigns this node a stable overlay index (1-based; the
// address plan maps index N → 10.42.N.0/24). Indexes are claimed
// lowest-free-first via CAS, so concurrent joiners resolve safely; a
// node that already holds an index reuses it across restarts.
//
// There is no index release: indexes are sparse by design (254 slots),
// and reuse-after-leave would let a stale peer's AllowedIPs point at
// the wrong node's traffic.
func ClaimIndex(ctx context.Context, st store.Store, nodeID string) (int, error) {
	for attempt := 0; attempt < 100; attempt++ {
		entries, err := st.List(ctx, "/network/nodeIndexes/")
		if err != nil {
			return 0, errors.New(errors.KindUnavailable, "mesh.claimIndex", err.Error())
		}
		held := make(map[int]bool, len(entries))
		for _, e := range entries {
			var idx int
			if _, scanErr := fmt.Sscanf(string(e.Key), "/network/nodeIndexes/%d", &idx); scanErr != nil {
				continue // not an index record
			}
			if string(e.Value) == nodeID {
				return idx, nil // already ours — stable across restarts
			}
			held[idx] = true
		}
		claimed := false
		for idx := 1; idx <= 254; idx++ {
			if held[idx] {
				continue
			}
			if _, err := st.CompareAndSwap(ctx, IndexKey(idx), 0, []byte(nodeID)); err == nil {
				return idx, nil
			}
			claimed = true // a conflict means someone else took it; re-scan
			break
		}
		if !claimed {
			return 0, errors.New(errors.KindUnavailable, "mesh.claimIndex",
				"no free overlay index (1–254)")
		}
	}
	return 0, errors.New(errors.KindConflict, "mesh.claimIndex",
		"could not claim an overlay index after 100 attempts")
}
