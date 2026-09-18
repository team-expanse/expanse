package agent

import (
	"context"
	"net"
	"path/filepath"
	"time"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/network/addrplan"
	"github.com/expanse/expanse/internal/network/mesh"
)

// lanIP returns this node's primary LAN address for the mesh endpoint:
// the host half of the recorded raft advertise address (the address
// peers were told to reach us on), falling back to the first
// non-loopback v4.
func (a *Agent) lanIP() string {
	bind, adv := control.LoadRaftAddr(a.cfg.DataDir)
	for _, cand := range []string{adv, bind} {
		if cand == "" {
			continue
		}
		if host, _, err := net.SplitHostPort(cand); err == nil && host != "0.0.0.0" {
			return host
		}
	}
	return control.LocalIP()
}

// meshLoop brings up the node's WireGuard identity and reconciles the
// exp0 peer set from the cluster store (§4.1). Cluster nodes only;
// witnesses skip (zero capacity, §4.9). Identity failure is logged and
// retried — a node whose store write raced startup must not crash-loop
// the whole daemon.
func (a *Agent) meshLoop(ctx context.Context) {
	var (
		ident *mesh.Identity
		ctrl  *mesh.LinuxController
		rec   *mesh.Reconciler
	)

	identity := func() bool {
		id, err := mesh.EnsureIdentity(ctx, a.store, a.cfg.NodeID,
			filepath.Join(a.cfg.DataDir, mesh.KeyRelPath),
			mesh.Endpoint(a.lanIP(), addrplan.WireGuardPort))
		if err != nil {
			a.logger.Warn("mesh identity unavailable; retrying", "err", err)
			return false
		}
		c, err := mesh.NewLinuxController(mesh.InterfaceName, addrplan.WireGuardPort, id.Private)
		if err != nil {
			a.logger.Warn("wgctrl unavailable; mesh disabled", "err", err)
			return true // permanent on this host — stop retrying
		}
		ident, ctrl = id, c
		rec = mesh.NewReconciler(a.store, c, mesh.Config{
			SelfNodeID: a.cfg.NodeID,
		})
		a.logger.Info("mesh identity ready",
			"public_key", ident.Peer.PublicKey,
			"overlay", ident.Peer.OverlayPrefix)
		return true
	}

	for {
		if ident == nil && identity() {
			defer ctrl.Close()
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}

	// Reconcile pass: immediately, then every 5 s — a join/leave shows
	// up in the mesh within two ticks, well inside G5.2's 30 s budget.
	if err := rec.Reconcile(ctx); err != nil {
		a.logger.Warn("first mesh reconcile failed", "err", err)
	}
	a.loop(ctx, 5*time.Second, "mesh", func() {
		// Re-publish the peer record every tick. The endpoint is derived
		// from the raft advertise address; at startup that file may not
		// exist yet (the first successful publish then records a
		// fallback LAN IP — on NAT'd test VMs the unroutable slirp
		// address, blackholing the node until restart). EnsureIdentity
		// CAS-refreshes the endpoint when it changed and is a no-op
		// otherwise; a tick cost of one store CAS buys DHCP-renewal
		// healing the docstring already promises.
		if _, err := mesh.EnsureIdentity(ctx, a.store, a.cfg.NodeID,
			filepath.Join(a.cfg.DataDir, mesh.KeyRelPath),
			mesh.Endpoint(a.lanIP(), addrplan.WireGuardPort)); err != nil {
			a.logger.Warn("mesh endpoint re-publish failed", "err", err)
		}
		if err := rec.Reconcile(ctx); err != nil {
			a.logger.Warn("mesh reconcile failed", "err", err)
		}
	})
}
