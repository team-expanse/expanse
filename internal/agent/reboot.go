package agent

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/blocks/controller"
	"github.com/expanse/expanse/internal/cluster/nodelc"
	"github.com/expanse/expanse/internal/storage/volume"
	"github.com/expanse/expanse/internal/store"
)

// stopKind is why the agent is stopping, which decides what it leaves behind.
type stopKind int

const (
	stopPlain   stopKind = iota // stopped for good, or removed: steps idle volumes down, as before
	stopRestart                 // started again on a running node: leaves every workload and volume as it is
	stopReboot                  // the node shuts down: VMs save themselves, replicas wait for the node
)

// stopKindOf classifies a stop from the node's state, systemd's job for the agent and removal.
func stopKindOf(systemStopping, restartJob, removed bool) stopKind {
	switch {
	case removed:
		return stopPlain
	case systemStopping:
		return stopReboot
	case restartJob:
		return stopRestart
	}
	return stopPlain
}

// volumeStopCause is how a stopping agent leaves its volumes: untouched for a restart.
func (k stopKind) volumeStopCause() error {
	if k == stopRestart {
		return volume.ErrRestart
	}
	return volume.ErrShutdown
}

// announceStop tells the cluster this node's agent will be back. After a reboot announcement
// its replicas keep their placements a while (controller.RebootHold) and its VMs resume their
// saves here; after a restart announcement the node stays healthy (nodelc.RestartHold), so
// nothing moves. Each ends once the node reports again. A plain stop announces nothing.
func (a *Agent) announceStop(k stopKind) {
	key, what, hold := nodelc.RestartKey(a.cfg.NodeID), "restart", nodelc.RestartHold
	switch k {
	case stopPlain:
		return
	case stopReboot:
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

// agentUnit is the agent's own systemd unit (nix/modules/agent.nix).
const agentUnit = "expansed.service"

// restartRequested reports whether systemd is restarting the agent's unit (a restart job, as a
// switch or `systemctl restart` makes), not stopping it for good.
func restartRequested() bool {
	out, _ := exec.Command("systemctl", "list-jobs", "--no-legend", agentUnit).Output()
	return restartJobIn(string(out), agentUnit)
}

// restartJobIn reads `systemctl list-jobs --no-legend` (job, unit, type, state per line).
func restartJobIn(out, unit string) bool {
	for line := range strings.Lines(out) {
		if f := strings.Fields(line); len(f) >= 3 && f[1] == unit && f[2] == "restart" {
			return true
		}
	}
	return false
}
