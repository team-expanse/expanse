package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

type fakeMembership struct {
	leader bool
	addr   string
	adds   int
}

func (f *fakeMembership) IsLeader() bool                { return f.leader }
func (f *fakeMembership) SelfAddress() (string, error)  { return f.addr, nil }
func (f *fakeMembership) AddVoter(_, addr string) error { f.adds++; f.addr = addr; return nil }

func advertiseRig(t *testing.T, recorded string) store.Store {
	t.Helper()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	b, _ := json.Marshal(join.NodeRecord{ID: "n1", Role: "voter", RaftAddr: recorded, JoinedAt: 1})
	if _, err := st.Put(context.Background(), store.Key(join.NodesKeyPrefix+"n1"), b); err != nil {
		t.Fatal(err)
	}
	return st
}

func recordedRaftAddr(t *testing.T, st store.Store) string {
	t.Helper()
	e, err := st.Get(context.Background(), store.Key(join.NodesKeyPrefix+"n1"))
	if err != nil {
		t.Fatal(err)
	}
	var r join.NodeRecord
	if err := json.Unmarshal(e.Value, &r); err != nil {
		t.Fatal(err)
	}
	return r.RaftAddr
}

func TestRepairMembershipMovesTheLeadersEntryAndRecord(t *testing.T) {
	ctx := context.Background()
	st := advertiseRig(t, "169.254.0.2:7444")
	m := &fakeMembership{addr: "169.254.0.2:7444"}

	if done, err := repairMembership(ctx, m, st, "n1", "192.168.10.154:7444"); done || err != nil || m.adds != 0 {
		t.Fatalf("a follower: done %v, err %v, adds %d; want it to wait for leadership", done, err, m.adds)
	}
	m.leader = true
	if done, err := repairMembership(ctx, m, st, "n1", "192.168.10.154:7444"); !done || err != nil {
		t.Fatalf("leader: done %v, err %v", done, err)
	}
	if m.addr != "192.168.10.154:7444" || recordedRaftAddr(t, st) != "192.168.10.154:7444" {
		t.Fatalf("raft has %q, the record %q", m.addr, recordedRaftAddr(t, st))
	}
	if done, _ := repairMembership(ctx, m, st, "n1", "192.168.10.154:7444"); !done || m.adds != 1 {
		t.Errorf("a repaired node changed membership again (adds %d)", m.adds)
	}
}

func TestRepairMembershipIsDoneWhenNothingIsStale(t *testing.T) {
	st := advertiseRig(t, "192.168.10.154:7444")
	m := &fakeMembership{addr: "192.168.10.154:7444"}
	if done, err := repairMembership(context.Background(), m, st, "n1", "192.168.10.154:7444"); !done || err != nil || m.adds != 0 {
		t.Fatalf("done %v, err %v, adds %d", done, err, m.adds)
	}
}
