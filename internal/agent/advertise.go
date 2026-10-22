package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/store"
)

// membership is the part of the raft store that holds this node's address.
type membership interface {
	IsLeader() bool
	SelfAddress() (string, error)
	AddVoter(id, addr string) error
}

// advertiseLoop moves this node's raft membership entry and node record to its advertise address,
// which RepairAdvertise may have changed at startup; raft membership changes only on the leader.
func (a *Agent) advertiseLoop(ctx context.Context) {
	for {
		done, err := repairMembership(ctx, a.ctl.store, a.store, a.cfg.NodeID, a.ctl.advertise)
		if err != nil {
			a.logger.Warn("advertise address repair", "err", err)
		}
		if done {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
	}
}

// repairMembership reports done once both the raft entry and the node record carry adv.
func repairMembership(ctx context.Context, m membership, st store.Store, id, adv string) (bool, error) {
	addr, err := m.SelfAddress()
	if err != nil {
		return false, err
	}
	if addr != "" && addr != adv {
		if !m.IsLeader() {
			return false, nil
		}
		if err := m.AddVoter(id, adv); err != nil { // same ID: raft updates its address
			return false, err
		}
	}
	key := store.Key(join.NodesKeyPrefix + id)
	e, err := st.Get(ctx, key)
	if err != nil {
		return false, err
	}
	var rec join.NodeRecord
	if err := json.Unmarshal(e.Value, &rec); err != nil {
		return false, err
	}
	if rec.RaftAddr == adv {
		return true, nil
	}
	rec.RaftAddr = adv
	b, err := json.Marshal(rec)
	if err != nil {
		return false, err
	}
	_, err = st.CompareAndSwap(ctx, key, e.Revision, b)
	return err == nil, err
}
