package volume

import (
	"context"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage/drbd"
)

// peerOf is a peer in the given connection, replication and disk state.
func peerOf(id int, conn drbd.Connection, repl drbd.Replication, disk drbd.DiskState) drbd.Peer {
	return drbd.Peer{
		NodeID: id, Name: "n" + string(rune('0'+id)), Connection: conn,
		Volumes: []drbd.PeerVolume{{Replication: repl, DiskState: disk}},
	}
}

func inSync(id int) drbd.Peer {
	return peerOf(id, drbd.ConnConnected, drbd.ReplEstablished, drbd.DiskUpToDate)
}

// checked is a running replica in the given role and disk state with the given peers.
func checked(t *testing.T, role drbd.Role, disk drbd.DiskState, peers ...drbd.Peer) *rig {
	t.Helper()
	r := newRig(t)
	reconcile(t, r, desired())
	r.drbd.role, r.drbd.disk, r.drbd.peers = role, disk, peers
	r.j.calls = nil
	return r
}

func refused(t *testing.T, err error) {
	t.Helper()
	if experrors.KindOf(err) != experrors.KindInvalid {
		t.Errorf("error %v, want a refusal (invalid)", err)
	}
}

func TestVerifyStartsWhenEveryReplicaIsInSync(t *testing.T) {
	r := checked(t, drbd.RolePrimary, drbd.DiskUpToDate, inSync(1), inSync(2))
	if err := r.rt.Verify(context.Background(), "vol-a1"); err != nil {
		t.Fatal(err)
	}
	if got := r.j.mutating(); len(got) != 1 || got[0] != "drbd.verify" {
		t.Errorf("calls %v, want only verify", got)
	}
}

func TestVerifyIsRefusedWhenTheVolumeIsNotFullyInSync(t *testing.T) {
	cases := map[string]struct {
		disk  drbd.DiskState
		peers []drbd.Peer
	}{
		"no peers":                       {drbd.DiskUpToDate, nil},
		"a peer not connected":           {drbd.DiskUpToDate, []drbd.Peer{inSync(1), peerOf(2, drbd.ConnConnecting, drbd.ReplOff, drbd.DiskUnknown)}},
		"a peer that only looks in sync": {drbd.DiskUpToDate, []drbd.Peer{peerOf(1, drbd.ConnConnecting, drbd.ReplEstablished, drbd.DiskUpToDate)}},
		"a peer reporting no volume":     {drbd.DiskUpToDate, []drbd.Peer{{NodeID: 1, Name: "n2", Connection: drbd.ConnConnected}}},
		"a peer resyncing":               {drbd.DiskUpToDate, []drbd.Peer{inSync(1), peerOf(2, drbd.ConnConnected, drbd.ReplSyncSource, drbd.DiskInconsistent)}},
		"a verify already runs":          {drbd.DiskUpToDate, []drbd.Peer{peerOf(1, drbd.ConnConnected, drbd.ReplVerifyS, drbd.DiskUpToDate)}},
		"a peer inconsistent":            {drbd.DiskUpToDate, []drbd.Peer{peerOf(1, drbd.ConnConnected, drbd.ReplEstablished, drbd.DiskInconsistent)}},
		"this replica behind":            {drbd.DiskInconsistent, []drbd.Peer{inSync(1)}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := checked(t, drbd.RolePrimary, tc.disk, tc.peers...)
			refused(t, r.rt.Verify(context.Background(), "vol-a1"))
			if got := r.j.mutating(); len(got) != 0 {
				t.Errorf("calls %v, want none", got)
			}
		})
	}
}

func TestVerifyOfADownResourceIsNotFound(t *testing.T) {
	r := checked(t, drbd.RolePrimary, drbd.DiskUpToDate, inSync(1))
	r.drbd.up = false
	if err := r.rt.Verify(context.Background(), "vol-a1"); experrors.KindOf(err) != experrors.KindNotFound {
		t.Errorf("error %v, want not found", err)
	}
}

func TestResyncInvalidatesASecondaryThatHasAnUpToDateSource(t *testing.T) {
	r := checked(t, drbd.RoleSecondary, drbd.DiskUpToDate, inSync(1))
	if err := r.rt.Resync(context.Background(), "vol-a1"); err != nil {
		t.Fatal(err)
	}
	if got := r.j.mutating(); len(got) != 1 || got[0] != "drbd.invalidate" {
		t.Errorf("calls %v, want only invalidate", got)
	}
}

func TestResyncNeedsOnlyOneUsableSource(t *testing.T) {
	r := checked(t, drbd.RoleSecondary, drbd.DiskUpToDate,
		peerOf(1, drbd.ConnConnecting, drbd.ReplOff, drbd.DiskUnknown), inSync(2))
	if err := r.rt.Resync(context.Background(), "vol-a1"); err != nil {
		t.Fatal(err)
	}
	if !r.j.has("drbd.invalidate") {
		t.Errorf("calls %v, want invalidate", r.j.calls)
	}
}

func TestResyncIsRefusedWhenItCouldLoseTheOnlyGoodCopy(t *testing.T) {
	cases := map[string]struct {
		role  drbd.Role
		disk  drbd.DiskState
		peers []drbd.Peer
	}{
		"the primary":             {drbd.RolePrimary, drbd.DiskUpToDate, []drbd.Peer{inSync(1)}},
		"no peers":                {drbd.RoleSecondary, drbd.DiskUpToDate, nil},
		"the only peer is down":   {drbd.RoleSecondary, drbd.DiskUpToDate, []drbd.Peer{peerOf(1, drbd.ConnConnecting, drbd.ReplOff, drbd.DiskUnknown)}},
		"the only peer is behind": {drbd.RoleSecondary, drbd.DiskUpToDate, []drbd.Peer{peerOf(1, drbd.ConnConnected, drbd.ReplSyncSource, drbd.DiskInconsistent)}},
		"already resyncing":       {drbd.RoleSecondary, drbd.DiskInconsistent, []drbd.Peer{inSync(1)}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := checked(t, tc.role, tc.disk, tc.peers...)
			refused(t, r.rt.Resync(context.Background(), "vol-a1"))
			if got := r.j.mutating(); len(got) != 0 {
				t.Errorf("calls %v, want none", got)
			}
		})
	}
}
