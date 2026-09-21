package volume

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage/drbd"
)

func peerIn(conn drbd.Connection) []drbd.Peer {
	return []drbd.Peer{{NodeID: 1, Name: "n2", Connection: conn}}
}

// diverged is a replica whose kernel dropped its connection after a split-brain.
func diverged(t *testing.T, conn drbd.Connection) *rig {
	t.Helper()
	r := newRig(t)
	reconcile(t, r, desired())
	r.drbd.peers = peerIn(conn)
	r.j.calls = nil
	return r
}

func rejoin(t *testing.T, r *rig, discard bool) {
	t.Helper()
	if err := r.rt.Rejoin(context.Background(), desired(), discard); err != nil {
		t.Fatalf("Rejoin: %v", err)
	}
}

func TestADiscardingReplicaReconnectsGivingUpItsData(t *testing.T) {
	r := diverged(t, drbd.ConnStandAlone)
	rejoin(t, r, true)
	if got := r.j.mutating(); len(got) != 1 || got[0] != "drbd.connect-discarding" {
		t.Errorf("calls %v, want only connect-discarding", got)
	}
}

func TestASurvivorReconnectsKeepingItsData(t *testing.T) {
	r := diverged(t, drbd.ConnStandAlone)
	rejoin(t, r, false)
	if got := r.j.mutating(); len(got) != 1 || got[0] != "drbd.connect" {
		t.Errorf("calls %v, want only connect", got)
	}
}

func TestADiscardingReplicaThatIsAlreadyConnectingIsDisconnectedFirst(t *testing.T) {
	r := diverged(t, drbd.ConnConnecting)
	rejoin(t, r, true)
	if r.j.index("drbd.disconnect") < 0 || r.j.index("drbd.disconnect") > r.j.index("drbd.connect-discarding") {
		t.Errorf("calls %v, want disconnect before connect-discarding", r.j.calls)
	}
}

func TestASurvivorLeavesConnectedPeersAlone(t *testing.T) {
	r := diverged(t, drbd.ConnConnected)
	rejoin(t, r, false)
	if got := r.j.mutating(); len(got) != 0 {
		t.Errorf("calls %v, want none", got)
	}
}

func TestADownDiscardingReplicaIsBroughtUpAndThenDiscards(t *testing.T) {
	r := diverged(t, drbd.ConnStandAlone)
	r.drbd.up = false
	r.drbd.peers = peerIn(drbd.ConnConnecting) // what starting leaves behind
	rejoin(t, r, true)
	up, off, connect := r.j.index("drbd.up"), r.j.index("drbd.disconnect"), r.j.index("drbd.connect-discarding")
	if up < 0 || off < up || connect < off {
		t.Errorf("calls %v, want up, disconnect, connect-discarding", r.j.calls)
	}
}

func TestADownSurvivorIsBroughtUpAndConnectsByItself(t *testing.T) {
	r := diverged(t, drbd.ConnStandAlone)
	r.drbd.up = false
	r.drbd.peers = peerIn(drbd.ConnConnecting)
	rejoin(t, r, false)
	if !r.j.has("drbd.up") || r.j.has("drbd.connect") {
		t.Errorf("calls %v, want up and no separate connect", r.j.calls)
	}
}

func TestRejoiningNeverAdjustsOrPromotes(t *testing.T) {
	for _, discard := range []bool{true, false} {
		r := diverged(t, drbd.ConnStandAlone)
		r.drbd.pending = true // an adjust would reconnect without the discard flag
		rejoin(t, r, discard)
		for _, c := range r.j.calls {
			if c == "drbd.adjust" || c == "drbd.primary" {
				t.Errorf("discard=%v: unexpected %s in %v", discard, c, r.j.calls)
			}
		}
	}
}

func TestRejoiningNeverCreatesTheBackingDevice(t *testing.T) {
	r := diverged(t, drbd.ConnStandAlone)
	delete(r.lvm.lvs, "vol-a1")
	err := r.rt.Rejoin(context.Background(), desired(), true)
	if experrors.KindOf(err) != experrors.KindNotFound {
		t.Errorf("err = %v, want not found", err)
	}
	for _, c := range r.j.calls {
		if c == "drbd.connect-discarding" || strings.HasPrefix(c, "lvm.create") {
			t.Errorf("unexpected %s in %v", c, r.j.calls)
		}
	}
}

func TestRejoiningRestoresAMissingConfig(t *testing.T) {
	r := diverged(t, drbd.ConnStandAlone)
	path := filepath.Join(r.dir, "vol-a1.res")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	rejoin(t, r, false)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("config not rewritten: %v", err)
	}
}
