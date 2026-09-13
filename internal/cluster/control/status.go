package control

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
)

// Report is the `expanse cluster status` payload (§5): name/ID, node
// count, quorum, leader, per-node info, generation, degraded.
type Report struct {
	ClusterID  string
	Name       string
	Version    string
	Leader     string // raft addr of the leader ("" when none)
	Generation int
	Degraded   bool // no leader → read-only (§4.10)
	QuorumNeed int
	QuorumHave int
	Nodes      []NodeStatus
}

// NodeStatus is one line of the status table.
type NodeStatus struct {
	ID       string
	RaftAddr string
	APIAddr  string
	Role     string
	State    string // leader | follower | nonvoter | witness
}

// WaitForLeader blocks until the store's raft node knows a leader
// (itself or a peer), or the timeout elapses.
func WaitForLeader(ctx context.Context, st *raftstore.Store, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for st.Leader() == "" {
		if ctx.Err() != nil || time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

// Status builds the report from a connected store plus the local raft
// view. QuorumHave is the number of voter node records reachable from
// the leader's configuration (all of them committed config entries) —
// an optimistic local view refined by liveness probes in T14.
func Status(ctx context.Context, st *raftstore.Store) (*Report, error) {
	rep := &Report{}

	metaEntry, err := st.Get(ctx, store.Key(MetaKey))
	switch {
	case errors.Is(err, errors.KindNotFound):
		// fallthrough: pre-meta cluster (shouldn't happen post-init)
	case err != nil:
		return nil, errors.Wrap(err, errors.KindUnavailable, "control.Status", "read meta: "+err.Error())
	default:
		var meta ClusterMeta
		if err := json.Unmarshal(metaEntry.Value, &meta); err == nil {
			rep.ClusterID, rep.Name, rep.Version = meta.ID, meta.Name, meta.Version
		}
	}

	if gen, err := st.Get(ctx, store.Key(GenerationKey)); err == nil {
		fmt.Sscanf(string(gen.Value), "%d", &rep.Generation)
	}

	entries, err := st.List(ctx, store.Key(join.NodesKeyPrefix))
	if err != nil {
		return nil, errors.Wrap(err, errors.KindUnavailable, "control.Status", "list nodes: "+err.Error())
	}
	rep.Degraded = !st.IsLeader() && st.Leader() == ""
	rep.Leader = st.Leader()
	voters := 0
	for _, e := range entries {
		var r join.NodeRecord
		if err := json.Unmarshal(e.Value, &r); err != nil {
			continue
		}
		role := r.Role
		if role == "" {
			role = "voter"
		}
		if role == "voter" {
			voters++
		}
		state := role
		if r.RaftAddr == rep.Leader && role == "voter" {
			state = "leader"
		}
		// Lifecycle annotation (§4.8): unreachable/failed from the
		// failure monitor, cordoned from drain/cordon.
		if r.State != "" {
			state += "/" + r.State
		}
		if r.Cordoned {
			state += "/cordoned"
		}
		rep.Nodes = append(rep.Nodes, NodeStatus{
			ID: r.ID, RaftAddr: r.RaftAddr, APIAddr: r.APIAddr, Role: role, State: state,
		})
	}
	rep.QuorumNeed = voters/2 + 1
	switch {
	case rep.Degraded:
		rep.QuorumHave = 0
	default:
		rep.QuorumHave = voters
	}
	return rep, nil
}

// Render formats the report as the CLI's text output.
func Render(r *Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "cluster:   %s (%s)\n", r.Name, r.ClusterID)
	fmt.Fprintf(&b, "version:   %s\n", r.Version)
	fmt.Fprintf(&b, "generation: %d\n", r.Generation)
	fmt.Fprintf(&b, "leader:    %s\n", orNone(r.Leader))
	fmt.Fprintf(&b, "quorum:    %d/%d%s\n", r.QuorumHave, r.QuorumNeed, degradedIf(r.Degraded))
	fmt.Fprintf(&b, "nodes:     %d\n", len(r.Nodes))
	fmt.Fprintf(&b, "\n  ID\tROLE\tSTATE\tRAFT\tAPI\n")
	for _, n := range r.Nodes {
		fmt.Fprintf(&b, "  %s\t%s\t%s\t%s\t%s\n", n.ID, n.Role, n.State, n.RaftAddr, n.APIAddr)
	}
	if r.Degraded {
		fmt.Fprintf(&b, "\nDEGRADED: no quorum — serving stale reads only (§4.10)\n")
	}
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func degradedIf(d bool) string {
	if d {
		return " (degraded)"
	}
	return ""
}

var (
	_ = os.Getenv
	_ = time.Now
	_ = store.OpPut
)
