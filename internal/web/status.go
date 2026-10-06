// Status pills: the one place raw wire enums (PHASE_RUNNING,
// VOLUME_STATE_DEGRADED, health=unhealthy) become a human label plus a
// colour tone, so every page renders the same word and colour for the
// same condition.
package web

import (
	"strings"

	"google.golang.org/grpc/status"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/nodelc"
	"github.com/expanse/expanse/internal/storage"
	pb "github.com/expanse/expanse/proto"
)

// pill is a status badge: Tone is one of ok, warn, crit, info, neutral
// and selects the CSS colour pair.
type pill struct {
	Label string
	Tone  string
}

var (
	pillUnknown = pill{Label: "Unknown", Tone: "neutral"}
	pillStale   = pill{Label: "Stale", Tone: "neutral"}
)

// phasePill humanizes a block/replica phase.
func phasePill(p pb.Phase) pill {
	switch p {
	case pb.Phase_RUNNING:
		return pill{"Running", "ok"}
	case pb.Phase_PENDING:
		return pill{"Pending", "info"}
	case pb.Phase_SCHEDULING:
		return pill{"Scheduling", "info"}
	case pb.Phase_PROVISIONING:
		return pill{"Provisioning", "info"}
	case pb.Phase_STARTING:
		return pill{"Starting", "info"}
	case pb.Phase_UPDATING:
		return pill{"Updating", "info"}
	case pb.Phase_DRAINING:
		return pill{"Draining", "warn"}
	case pb.Phase_DEGRADED:
		return pill{"Degraded", "warn"}
	case pb.Phase_TERMINATING:
		return pill{"Terminating", "warn"}
	case pb.Phase_FAILED:
		return pill{"Failed", "crit"}
	case pb.Phase_LOST:
		return pill{"Lost", "crit"}
	case pb.Phase_TERMINATED:
		return pill{"Terminated", "neutral"}
	default:
		return pillUnknown
	}
}

// volumePill humanizes a storage.VolumeState.
func volumePill(s storage.VolumeState) pill {
	switch s {
	case storage.StateHealthy:
		return pill{"Healthy", "ok"}
	case storage.StateCreating:
		return pill{"Creating", "info"}
	case storage.StateResyncing:
		return pill{"Resyncing", "info"}
	case storage.StateDeleting:
		return pill{"Deleting", "info"}
	case storage.StateDegraded:
		return pill{"Degraded", "warn"}
	case storage.StateUnderReplicated:
		return pill{"Under-replicated", "warn"}
	case storage.StateReadOnly:
		return pill{"Read-only", "warn"}
	case storage.StateFailed:
		return pill{"Failed", "crit"}
	case storage.StateNeedsManualRecovery:
		return pill{"Needs manual recovery", "crit"}
	default:
		return pillUnknown
	}
}

// healthPill humanizes a pb.Health check/resource status.
func healthPill(h pb.Health) pill {
	switch h {
	case pb.Health_HEALTH_HEALTHY:
		return pill{"Healthy", "ok"}
	case pb.Health_HEALTH_DEGRADED:
		return pill{"Degraded", "warn"}
	case pb.Health_HEALTH_UNHEALTHY:
		return pill{"Unhealthy", "crit"}
	default:
		return pillUnknown
	}
}

// heartbeatPill shows a node's last heartbeat, which is stale once the node is unreachable or failed.
func heartbeatPill(n control.NodeStatus, status string) pill {
	if n.Lifecycle == nodelc.StateUnreachable || n.Lifecycle == nodelc.StateFailed {
		return pillStale
	}
	return nodeHealthPill(status)
}

// nodePill condenses a node's lifecycle, self-reported degradation and
// cordon or drain into one badge, worst condition first.
func nodePill(n control.NodeStatus) pill {
	switch {
	case n.Lifecycle == nodelc.StateFailed:
		return pill{"Failed", "crit"}
	case n.Lifecycle == nodelc.StateUnreachable:
		return pill{"Unreachable", "warn"}
	case n.Degraded:
		return pill{"Degraded", "warn"}
	case strings.Contains(n.State, "draining"):
		return pill{"Draining", "warn"}
	case strings.Contains(n.State, "cordoned"):
		return pill{"Cordoned", "neutral"}
	default:
		return pill{"Online", "ok"}
	}
}

// nodeHealthPill parses the "health=<x> ..." value each agent writes to
// /nodes/<id>/status every 10s, the only cluster-wide per-node health
// signal.
func nodeHealthPill(status string) pill {
	for _, f := range strings.Fields(status) {
		if v, ok := strings.CutPrefix(f, "health="); ok {
			switch v {
			case "healthy":
				return pill{"Healthy", "ok"}
			case "degraded":
				return pill{"Degraded", "warn"}
			case "unhealthy":
				return pill{"Unhealthy", "crit"}
			}
		}
	}
	return pillUnknown
}

// agentPill humanizes the reconciler's NodeStatus.
func agentPill(s pb.NodeStatus_Status) pill {
	switch s {
	case pb.NodeStatus_STATUS_IDLE:
		return pill{"Idle", "ok"}
	case pb.NodeStatus_STATUS_RECONCILING:
		return pill{"Reconciling", "info"}
	case pb.NodeStatus_STATUS_DEGRADED:
		return pill{"Degraded", "warn"}
	case pb.NodeStatus_STATUS_ERROR:
		return pill{"Error", "crit"}
	default:
		return pillUnknown
	}
}

// roleLabel humanizes a raft membership role ("" is a voter, as
// control.Status treats it).
func roleLabel(role string) string {
	switch role {
	case "witness":
		return "Witness"
	case "nonvoter":
		return "Non-voter"
	default:
		return "Voter"
	}
}

// isLeader reports whether control.Status flagged this node's state as
// the leader (state is "leader" plus optional "/annotation" suffixes).
func isLeader(n control.NodeStatus) bool {
	return n.State == "leader" || strings.HasPrefix(n.State, "leader/")
}

// errText is an error as an operator reads it: a gRPC status's message without the "rpc error: code = …" prefix.
func errText(err error) string {
	if s, ok := status.FromError(err); ok {
		return s.Message()
	}
	return err.Error()
}
