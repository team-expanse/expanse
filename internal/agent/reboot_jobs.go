package agent

import (
	"context"
	"os"
	"os/exec"
	"strings"

	"github.com/expanse/expanse/internal/blocks/controller"
	"github.com/expanse/expanse/internal/cluster/nodelc"
	"github.com/expanse/expanse/internal/store/raftstore"
	"github.com/expanse/expanse/internal/upgrade/rollout"
)

// rebooter is the leader's side of a node reboot: drain the node, measure the drain, and put
// the node back in service once it is back.
type rebooter interface {
	begin(ctx context.Context, r rollout.Reboot) (rollout.Reboot, error)
	progress(ctx context.Context, r rollout.Reboot) (rollout.Drain, error)
	end(ctx context.Context, r rollout.Reboot) error
}

// clusterRebooter drains through nodelc and the block controller's view of placements.
type clusterRebooter struct{ st *raftstore.Store }

// begin drains the node, or reboots it in place when no other node could take its replicas.
func (c clusterRebooter) begin(ctx context.Context, r rollout.Reboot) (rollout.Reboot, error) {
	others, err := otherPlaceableNodes(ctx, c.st, r.Node)
	if err != nil {
		return r, err
	}
	if !others {
		r.InPlace, r.State = true, rollout.RebootRebooting
		return r, nil
	}
	replicas, err := controller.NodeReplicas(ctx, c.st, r.Node)
	if err != nil {
		return r, err
	}
	cordoned, err := nodelc.DrainForReboot(ctx, c.st, r.Node)
	if err != nil {
		return r, err
	}
	// A retried drain finds the node drained already; r keeps the cordon recorded the first time.
	r.Cordoned, r.State, r.Replicas = r.Cordoned || cordoned, rollout.RebootDraining, nil
	for _, p := range replicas {
		r.Replicas = append(r.Replicas, rollout.Replica{Block: p.Block, Index: p.Index})
	}
	return r, nil
}

func (c clusterRebooter) progress(ctx context.Context, r rollout.Reboot) (rollout.Drain, error) {
	was := make([]controller.NodeReplica, 0, len(r.Replicas))
	for _, p := range r.Replicas {
		was = append(was, controller.NodeReplica{Block: p.Block, Index: p.Index})
	}
	moving, held, err := controller.DrainProgress(ctx, c.st, r.Node, was)
	if err != nil {
		return rollout.Drain{}, err
	}
	var d rollout.Drain
	for _, p := range moving {
		d.Moving = append(d.Moving, rollout.Replica{Block: p.Block, Index: p.Index}.String())
	}
	for p, why := range held {
		if d.Held == nil {
			d.Held = map[string]string{}
		}
		d.Held[rollout.Replica{Block: p.Block, Index: p.Index}.String()] = why
	}
	return d, nil
}

func (c clusterRebooter) end(ctx context.Context, r rollout.Reboot) error {
	return nodelc.EndDrainForReboot(ctx, c.st, r.Node, r.Cordoned)
}

// otherPlaceableNodes reports whether any node but node can take replicas now.
func otherPlaceableNodes(ctx context.Context, st *raftstore.Store, node string) (bool, error) {
	infos, err := nodelc.List(ctx, st)
	if err != nil {
		return false, err
	}
	for _, in := range infos {
		if in.ID != node && in.Role != "witness" && !in.Cordoned && strings.HasPrefix(in.Lifecycle, "healthy") {
			return true, nil
		}
	}
	return false, nil
}

// bootID reads the kernel's random boot ID, "" when unreadable.
func bootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// rebootNode asks systemd to reboot, so the agent's stop announces it and VMs save themselves.
func rebootNode(ctx context.Context) error {
	return exec.CommandContext(ctx, "systemctl", "reboot").Run()
}
