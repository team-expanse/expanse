package runtime

import "github.com/expanse/expanse/internal/storage"

// reconcileRoles makes the published placement say what the coordinator knows: a
// replica it marked Stale is Stale, and one it holds live in the fan-out that is
// still published Stale (a resync whose outcome was never recorded) is a healthy
// Secondary again. It reports whether anything changed. Replicas mid-resync and
// replicas the coordinator does not hold are left to their own paths.
func reconcileRoles(pl []storage.Replica, self string, stale, live map[string]bool, resyncing func(string) bool) bool {
	changed := false
	for i := range pl {
		r := &pl[i]
		if r.NodeID == self || resyncing(r.NodeID) {
			continue
		}
		switch {
		case stale[r.NodeID] && r.Role != storage.RoleStale:
			r.Role, r.Healthy, changed = storage.RoleStale, false, true
		case live[r.NodeID] && !stale[r.NodeID] && r.Role == storage.RoleStale:
			r.Role, r.Healthy, changed = storage.RoleSecondary, true, true
		}
	}
	return changed
}
