package agent

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/blocks/controller"
	"github.com/expanse/expanse/internal/cluster/nodelc"
	"github.com/expanse/expanse/internal/store"
)

// announceStop tells the cluster this node's agent is stopping to come back. A node shutdown is
// a reboot: its replicas keep their placements a while (controller.RebootHold) and its VMs resume
// their saves here. Any other stop is an agent restart, which leaves the node's workloads running:
// the node stays healthy for nodelc.RestartHold, so nothing moves. Each announcement ends once the
// node reports again.
func (a *Agent) announceStop(stopping func() bool) {
	key, what, hold := nodelc.RestartKey(a.cfg.NodeID), "restart", nodelc.RestartHold
	if stopping() {
		key, what, hold = controller.RebootKey(a.cfg.NodeID), "reboot", controller.RebootHold
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := []byte(time.Now().UTC().Format(time.RFC3339))
	_, err := a.store.Put(ctx, store.Key(key), now)
	switch {
	case a.logger == nil:
	case err != nil:
		a.logger.Warn("could not announce this node's "+what+"; its replicas may move", "err", err)
	default:
		a.logger.Info("announced this node's "+what+"; its replicas stay placed", "hold", hold)
	}
}

// systemStopping reports whether the node is shutting down or rebooting.
func systemStopping() bool {
	out, _ := exec.Command("systemctl", "is-system-running").Output() // exits non-zero unless running
	return strings.TrimSpace(string(out)) == "stopping"
}
