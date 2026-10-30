package agent

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/blocks/controller"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// announceReboot tells the controller this node is going down to come back, so its replicas keep
// their placements a while (controller.RebootHold) and its VMs resume their saves here.
func (a *Agent) announceReboot(stopping func() bool) {
	if !stopping() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := []byte(time.Now().UTC().Format(time.RFC3339))
	if _, err := a.store.Put(ctx, store.Key(controller.RebootKey(a.cfg.NodeID)), now); err != nil && a.logger != nil {
		a.logger.Warn("could not announce this node's reboot; its replicas may move", "err", err)
	}
}

// clearReboot withdraws the announcement once this node is back, retrying until the store takes it.
func (a *Agent) clearReboot(ctx context.Context, retry time.Duration) {
	for {
		err := a.store.Delete(ctx, store.Key(controller.RebootKey(a.cfg.NodeID)), 0)
		if err == nil || errors.Is(err, errors.KindNotFound) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

// systemStopping reports whether the node is shutting down or rebooting.
func systemStopping() bool {
	out, _ := exec.Command("systemctl", "is-system-running").Output() // exits non-zero unless running
	return strings.TrimSpace(string(out)) == "stopping"
}
