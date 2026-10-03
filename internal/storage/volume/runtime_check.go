package volume

import (
	"context"
	"slices"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage/drbd"
)

// Verify starts an online verify of this replica against every peer. It refuses
// unless every replica is in sync, since a verify of one that is resyncing would
// report the resync as damage.
func (r *Runtime) Verify(ctx context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := r.DRBD.Status(ctx, name)
	if err != nil {
		return err
	}
	replicas := slices.DeleteFunc(slices.Clone(st.Peers), isDiskless)
	if len(replicas) == 0 || !localUpToDate(st) || slices.ContainsFunc(replicas, func(p drbd.Peer) bool { return !peerInSync(p) }) {
		return experrors.New(experrors.KindInvalid, "volume.Verify", "every replica must be connected and in sync to verify")
	}
	return r.DRBD.Verify(ctx, name)
}

// Resync throws this replica's data away and copies it again from its peers. It
// refuses on the primary, and unless this replica is itself in sync and a peer is
// in sync to copy from: otherwise the discarded data could be the last good copy.
func (r *Runtime) Resync(ctx context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := r.DRBD.Status(ctx, name)
	if err != nil {
		return err
	}
	switch {
	case st.Role == drbd.RolePrimary:
		return experrors.New(experrors.KindInvalid, "volume.Resync", "the primary cannot be resynced; move the primary first")
	case !localUpToDate(st):
		return experrors.New(experrors.KindInvalid, "volume.Resync", "this replica is not in sync, so it is already being rebuilt or has no data to replace")
	case !slices.ContainsFunc(st.Peers, peerInSync):
		return experrors.New(experrors.KindInvalid, "volume.Resync", "no connected replica in sync to copy from")
	}
	return r.DRBD.Invalidate(ctx, name)
}

func localUpToDate(st *drbd.Status) bool {
	return len(st.Volumes) > 0 && !slices.ContainsFunc(st.Volumes, func(v drbd.Volume) bool { return v.DiskState != drbd.DiskUpToDate })
}

// peerInSync reports whether a peer is connected, holds UpToDate data and has no resync or verify running.
func peerInSync(p drbd.Peer) bool {
	return p.Connection == drbd.ConnConnected && len(p.Volumes) > 0 &&
		!slices.ContainsFunc(p.Volumes, func(v drbd.PeerVolume) bool {
			return v.Replication != drbd.ReplEstablished || v.DiskState != drbd.DiskUpToDate
		})
}

// isDiskless reports a connected tiebreaker: a peer whose every volume is diskless.
func isDiskless(p drbd.Peer) bool {
	return len(p.Volumes) > 0 && !slices.ContainsFunc(p.Volumes, func(v drbd.PeerVolume) bool { return v.DiskState != drbd.DiskDiskless })
}
