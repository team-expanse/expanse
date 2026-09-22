package agent

// The web UI's own VIP (ROADMAP.md Phase 2, A3/D8): a bespoke holder,
// not driven by scanBlocks. The UI is a control-plane service present
// uniformly on every non-witness node (agent.go starts it unconditionally),
// not a block a user deploys — so membership itself is readiness, unlike a
// block VIP's ready-replica count. Its cert's shared SAN
// (ca.UIVIPHostname, D9) means whichever node holds this address presents
// a name a client already trusts, without needing to know which node that
// is.

import (
	"context"
	"encoding/json"
	"io"
	"net/netip"
	"strings"

	"github.com/expanse/expanse/internal/network/vip"
	"github.com/expanse/expanse/internal/store"
)

// uiVIPRef names the UI's allocation record (vip.PoolKey) and holder
// lease/block field — not a real block ref, but the same store
// mechanism a block's VIP uses.
const uiVIPRef = "expanse-ui"

// noopCloser satisfies io.Closer for the UI VIP's Seams.Listen: unlike a
// block VIP, there is nothing to start or stop here. The web UI server
// (agent.go) already listens on 0.0.0.0:config.PortUI on this node;
// once the VIP address is added to the interface, that same listener
// starts answering it for free.
type noopCloser struct{}

func (noopCloser) Close() error { return nil }

func (a *Agent) uiVIPLoop(ctx context.Context) {
	if err := a.initVIP(); err != nil {
		a.logger.Error("ui vip disabled", "err", err)
		return
	}
	scope := vip.ScopeInternal
	pool := vip.InternalPool()
	if len(a.extPool) > 0 {
		scope = vip.ScopeExternal
		pool = a.extPool
	}
	pfx, err := vip.Allocate(ctx, a.store, uiVIPRef, scope, pool)
	if err != nil {
		a.logger.Error("ui vip allocation failed", "err", err)
		return
	}
	seams, err := a.vipSeams()
	if err != nil {
		a.logger.Warn("ui vip seams unavailable", "err", err)
		return
	}
	seams.Listen = func(netip.Prefix) (io.Closer, error) { return noopCloser{}, nil }

	h := vip.NewHolder(vip.HolderConfig{
		Leases:     a.leaseManager(),
		Self:       a.cfg.NodeID,
		VIP:        pfx,
		Block:      uiVIPRef,
		Logger:     a.logger.With("component", "ui-vip"),
		Cands:      a.uiVIPCandidates,
		Seams:      seams,
		OnAcquired: func(p netip.Prefix) { a.publishVIPHolder(p, a.cfg.NodeID) },
		OnLost:     func(p netip.Prefix) { a.publishVIPHolder(p, "") },
	})
	if err := h.Run(ctx); err != nil {
		a.logger.Warn("ui vip holder exited", "err", err)
	}
}

// uiVIPCandidates returns every non-witness cluster member: the UI
// runs on all of them uniformly, so membership itself is the readiness
// signal (contrast scanBlocks' readyCandidates, which counts RUNNING
// placements). A dead member is not filtered out here — its lease
// simply stops renewing and a survivor takes over on expiry, the same
// liveness proof block VIPs rely on (Holder.Run's comment).
func (a *Agent) uiVIPCandidates() []vip.Candidate {
	entries, err := a.store.List(store.WithStale(context.Background()), "/nodes/")
	if err != nil {
		a.logger.Warn("ui vip candidate scan failed", "err", err)
		return nil
	}
	var out []vip.Candidate
	for _, e := range entries {
		rest := strings.TrimPrefix(string(e.Key), "/nodes/")
		if rest == "" || strings.Contains(rest, "/") {
			continue // a /nodes/<id>/status or /health sub-key, not the record itself
		}
		var rec struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(e.Value, &rec) != nil || rec.Role == "witness" {
			continue
		}
		out = append(out, vip.Candidate{NodeID: rest, ReadyReplicas: 1})
	}
	return out
}
