package rollout

import (
	"fmt"
	"slices"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/storage"
)

// ClusterHealth is the gate between nodes: a leader, every node reachable and in quorum, and
// every volume settled (UnderReplicated counts: it is as replicated as the cluster allows).
func ClusterHealth(rep *control.Report, volumes map[string]storage.VolumeState) Health {
	if rep == nil || rep.Leader == "" {
		return Health{Why: "no leader"}
	}
	for _, n := range rep.Nodes {
		if n.Lifecycle != "" {
			return Health{Why: fmt.Sprintf("node %s is %s", n.ID, n.Lifecycle)}
		}
		if n.Degraded {
			return Health{Why: fmt.Sprintf("node %s has no quorum", n.ID)}
		}
	}
	names := make([]string, 0, len(volumes))
	for name := range volumes {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if st := volumes[name]; st != storage.StateHealthy && st != storage.StateUnderReplicated {
			return Health{Why: fmt.Sprintf("volume %s is %s", name, st)}
		}
	}
	return Health{OK: true}
}
