package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/cluster/nodelc"
	"github.com/expanse/expanse/internal/config"
	"github.com/expanse/expanse/internal/store"
)

// ErrRemoved stops the agent of a node that was removed from its cluster.
var ErrRemoved = errors.New("this node was removed from the cluster")

// ExitRemoved is the agent's exit status once removed; the unit does not restart on it.
const ExitRemoved = 78

// removedMarker, in the data dir, keeps a removed node's agent from starting again.
const removedMarker = "removed.json"

// removalPoll is how often the agent looks for its own revocation.
var removalPoll = 5 * time.Second

// removalAsk is how often the agent asks its peers whether it was removed.
var removalAsk = 30 * time.Second

// watchRemoval looks for this node's revocation in the local store copy, which a
// removed node still has, and else asks its peers; it retires the node once found.
func (a *Agent) watchRemoval(ctx context.Context) {
	t := time.NewTicker(removalPoll)
	defer t.Stop()
	var asked time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rev := a.revocation(ctx)
		if rev == nil && a.askPeers != nil && time.Since(asked) >= removalAsk {
			asked = time.Now()
			rev = a.peersRevocation(ctx)
		}
		if rev != nil {
			a.retire(ctx, rev)
			return
		}
	}
}

func (a *Agent) revocation(ctx context.Context) *nodelc.Revocation {
	e, err := a.store.Get(store.WithStale(ctx), store.Key(nodelc.RevokedKeyPrefix+a.cfg.NodeID))
	if err != nil {
		return nil
	}
	return decodeRevocation(a.cfg.NodeID, e.Value)
}

func (a *Agent) peersRevocation(ctx context.Context) *nodelc.Revocation {
	rev, err := a.askPeers(ctx)
	if err != nil {
		a.logger.Debug("could not ask peers whether this node was removed", "err", err)
	}
	if rev != nil {
		a.logger.Warn("a peer says this node was removed while it was cut off")
	}
	return rev
}

func decodeRevocation(id string, raw []byte) *nodelc.Revocation {
	var rev nodelc.Revocation
	if json.Unmarshal(raw, &rev) != nil {
		rev = nodelc.Revocation{NodeID: id}
	}
	return &rev
}

// askRemoval asks the peers of a node without quorum whether it was removed: one
// cut off while it was removed never receives the revocation itself.
func (c *clusterCtl) askRemoval(ctx context.Context) (*nodelc.Revocation, error) {
	if !c.store.Degraded() {
		return nil, nil
	}
	peers, err := c.store.PeerAddrs()
	if err != nil {
		return nil, err
	}
	var addrs []string
	for _, p := range peers {
		if host, _, err := net.SplitHostPort(p); err == nil {
			addrs = append(addrs, net.JoinHostPort(host, strconv.Itoa(config.PortJoin)))
		}
	}
	tlsCfg, err := control.InternalClientTLS(ctx, c.store, c.ca, c.dataDir)
	if err != nil {
		return nil, err
	}
	raw, err := join.AskRemoval(ctx, addrs, c.store.NodeID(), tlsCfg)
	if err != nil || raw == nil {
		return nil, err
	}
	return decodeRevocation(c.store.NodeID(), raw), nil
}

// retire stops every workload this node runs and the agent with it: the rest of the
// cluster has already replaced them, so running on would duplicate them.
func (a *Agent) retire(ctx context.Context, rev *nodelc.Revocation) {
	a.logger.Error("this node was removed from the cluster; stopping its workloads and the agent",
		"by", rev.By, "reason", rev.Reason)
	a.status.Store("removed")
	a.recon.Retire(ctx)
	if err := writeRemovedMarker(a.cfg.DataDir, rev); err != nil {
		a.logger.Error("removed marker not written", "err", err)
	}
	a.removed.Store(true)
	a.Shutdown("removed from the cluster")
}

func writeRemovedMarker(dir string, rev *nodelc.Revocation) error {
	raw, err := json.Marshal(rev)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, removedMarker), raw, 0o600)
}

// RemovedFrom reads the removal recorded in a data dir, or nil when there is none.
func RemovedFrom(dir string) (*nodelc.Revocation, error) {
	raw, err := os.ReadFile(filepath.Join(dir, removedMarker))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rev nodelc.Revocation
	if err := json.Unmarshal(raw, &rev); err != nil {
		return nil, fmt.Errorf("%s: %w", removedMarker, err)
	}
	return &rev, nil
}

// removedError explains a removal and what brings the machine back.
func removedError(dir string, rev *nodelc.Revocation) error {
	why := ""
	if rev.Reason != "" {
		why = " (" + rev.Reason + ")"
	}
	return fmt.Errorf("%w by %s%s; its identity is revoked, so reinstall the machine to add it back (%s records the removal)",
		ErrRemoved, rev.By, why, filepath.Join(dir, removedMarker))
}
