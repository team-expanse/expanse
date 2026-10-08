package agent

// Tests for the §4.2 candidate extraction: a placement counts as a VIP
// candidate exactly while its phase is RUNNING. Liveness is not part of
// this gate — a dead holder's failure is proven by its lease expiring
// (Holder.Read of the recorded lease), not by placement bookkeeping.

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	pb "github.com/expanse/expanse/proto"

	"github.com/expanse/expanse/internal/network/vip"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

func blockWithPlacements(nodes []string, phases ...pb.Phase) *pb.Block {
	b := &pb.Block{Status: &pb.BlockStatus{}}
	for i, n := range nodes {
		phase := pb.Phase_RUNNING
		if i < len(phases) {
			phase = phases[i]
		}
		b.Status.Placements = append(b.Status.Placements,
			&pb.PlacementStatus{ReplicaIndex: int32(i), NodeId: n, Phase: phase})
	}
	return b
}

func TestReadyCandidates(t *testing.T) {
	t.Run("running placements count, once per node", func(t *testing.T) {
		b := blockWithPlacements([]string{"n1", "n2"})
		got := readyCandidates(b)
		if len(got) != 2 {
			t.Fatalf("want 2 candidates, got %+v", got)
		}
	})

	t.Run("non-running placements are skipped", func(t *testing.T) {
		b := blockWithPlacements([]string{"n1", "n2", "n3"},
			pb.Phase_RUNNING, pb.Phase_PENDING, pb.Phase_FAILED)
		got := readyCandidates(b)
		if len(got) != 1 || got[0].NodeID != "n1" {
			t.Fatalf("want only n1, got %+v", got)
		}
	})

	t.Run("no placements means no candidates", func(t *testing.T) {
		if got := readyCandidates(&pb.Block{Status: &pb.BlockStatus{}}); len(got) != 0 {
			t.Fatalf("want no candidates, got %+v", got)
		}
	})
}

// listFailingStore wraps a real store.Store and makes every List call
// fail, simulating a transient raft read failure (e.g. "ForwardRead:
// linear read rpc failed" during a leadership change) without needing a
// real cluster.
type listFailingStore struct {
	store.Store
}

func (s *listFailingStore) List(ctx context.Context, prefix store.Key) ([]*store.Entry, error) {
	return nil, context.DeadlineExceeded
}

func newTestAgent(t *testing.T, st store.Store) *Agent {
	t.Helper()
	return &Agent{
		store:    st,
		logger:   slog.Default(),
		holders:  map[string]*holderRun{},
		vipCands: map[string][]vip.Candidate{},
	}
}

// TestScanBlocksReturnsErrorOnListFailure pins the contract vipPass
// relies on: a failed list must be distinguishable from "no blocks have
// a VIP" (both used to come back as the same empty, nil-error result).
func TestScanBlocksReturnsErrorOnListFailure(t *testing.T) {
	st, err := boltstore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("boltstore.New: %v", err)
	}
	defer st.Close()

	a := newTestAgent(t, &listFailingStore{Store: st})
	desired, cands, err := a.scanBlocks(context.Background())
	if err == nil {
		t.Fatal("scanBlocks: want an error on a failed list, got nil")
	}
	if desired != nil || cands != nil {
		t.Fatalf("scanBlocks: want nil maps alongside the error, got desired=%v cands=%v", desired, cands)
	}
}

// TestVIPPassKeepsExistingHoldersOnScanFailure is the share-smb-failover
// .nix finding: vipPass used to treat a scan failure the same as "every
// VIP disappeared," tearing down (hard-canceling) every live holder on
// this node on any transient raft read blip. A holder recreated fresh
// after that has no memory of prior state and races the truly preferred
// candidate purely on recreate timing -- reproduced directly as the
// cluster's VIP flapping between nodes for minutes after a crash.
func TestVIPPassKeepsExistingHoldersOnScanFailure(t *testing.T) {
	st, err := boltstore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("boltstore.New: %v", err)
	}
	defer st.Close()

	a := newTestAgent(t, &listFailingStore{Store: st})
	canceled := false
	done := make(chan struct{})
	close(done) // pre-closed: a spurious cancel+wait would not hang the test
	a.holders["vip:192.168.1.101/32"] = &holderRun{cancel: func() { canceled = true }, done: done}
	a.vipCands["vip:192.168.1.101/32"] = []vip.Candidate{{NodeID: "n3", ReadyReplicas: 1}}

	a.vipPass(context.Background())

	if canceled {
		t.Fatal("vipPass tore down an existing holder on a scan failure")
	}
	if _, ok := a.holders["vip:192.168.1.101/32"]; !ok {
		t.Fatal("vipPass removed an existing holder from a.holders on a scan failure")
	}
	if len(a.vipCands["vip:192.168.1.101/32"]) == 0 {
		t.Fatal("vipPass wiped vipCands on a scan failure")
	}
}

func TestVIPPortsListsEveryExposedPortWithItsTarget(t *testing.T) {
	b := &pb.Block{Spec: &pb.BlockSpec{Network: &pb.Network{Ports: []*pb.Port{
		{Name: "http", Port: 80, TargetPort: 18080, Expose: pb.Expose_EXPOSE_VIP},
		{Name: "admin", Port: 2019, TargetPort: 2019},
		{Name: "https", Port: 443, TargetPort: 18443, Expose: pb.Expose_EXPOSE_VIP},
		{Name: "dns", Port: 53, Expose: pb.Expose_EXPOSE_VIP}, // no target: same port
	}}}}
	got := vipPorts(b)
	want := []vipPort{{exposed: 80, target: 18080}, {exposed: 443, target: 18443}, {exposed: 53, target: 53}}
	if len(got) != len(want) {
		t.Fatalf("vipPorts = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("vipPorts[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if vipPorts(&pb.Block{}) != nil {
		t.Errorf("a block without ports has VIP ports")
	}
}
