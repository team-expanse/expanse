package volume

import (
	"slices"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/storage/drbd"
)

// Replica is one member's storage.Replica as this node's kernel reports it.
type Replica struct {
	storage.Replica
}

// Observed is this node's view of a volume's members. A member it cannot see has
// role "" and is unhealthy; Healthy means current data and, for this node, quorum.
type Observed struct {
	Replicas []Replica // by node-id, this node included
	Quorum   bool
}

// Observe maps DRBD's local status onto the members' replicas. Peers that are not
// members (a retired id still lingering in the kernel) are ignored.
func Observe(st *drbd.Status, self string, members []drbd.Member) Observed {
	members = slices.SortedFunc(slices.Values(members), func(a, b drbd.Member) int { return a.NodeID - b.NodeID })
	o := Observed{Quorum: len(st.Volumes) > 0 && st.HasQuorum()}
	for _, m := range members {
		r := Replica{Replica: storage.Replica{NodeID: m.Host}}
		if m.Host == self {
			r = observeSelf(st, r)
		} else if p, ok := findPeer(st, m.NodeID); ok {
			r = observePeer(p, r)
		}
		o.Replicas = append(o.Replicas, r)
	}
	return o
}

func findPeer(st *drbd.Status, id int) (drbd.Peer, bool) {
	for _, p := range st.Peers {
		if p.NodeID == id {
			return p, true
		}
	}
	return drbd.Peer{}, false
}

// observeSelf resyncs when a peer is the source, and is healthy only with current data and quorum.
func observeSelf(st *drbd.Status, r Replica) Replica {
	disk := worstDisk(st.Volumes, func(v drbd.Volume) drbd.DiskState { return v.DiskState })
	var targets []drbd.PeerVolume
	for _, p := range st.Peers {
		targets = append(targets, inState(p.Volumes, drbd.ReplSyncTarget, drbd.ReplPausedSyncT)...)
	}
	r.Role = roleOf(st.Role, disk, len(targets) > 0)
	r.Healthy = disk == drbd.DiskUpToDate && st.HasQuorum()
	r.SyncPercent, r.OutOfSyncKiB = slowest(targets)
	return r
}

// observePeer resyncs when this node is the source, and carries what a verify found; a
// disconnected peer's last disk state and count are stale, so they are not read.
func observePeer(p drbd.Peer, r Replica) Replica {
	if p.Connection != drbd.ConnConnected {
		return r
	}
	disk := worstDisk(p.Volumes, func(v drbd.PeerVolume) drbd.DiskState { return v.DiskState })
	targets := inState(p.Volumes, drbd.ReplSyncSource, drbd.ReplPausedSyncS)
	r.Role = roleOf(p.Role, disk, len(targets) > 0)
	r.Healthy = disk == drbd.DiskUpToDate
	r.SyncPercent, _ = slowest(targets)
	r.OutOfSyncKiB = outOfSync(p.Volumes)
	r.Verifying = len(inState(p.Volumes, drbd.ReplVerifyS, drbd.ReplVerifyT)) > 0
	return r
}

func outOfSync(vols []drbd.PeerVolume) (kib uint64) {
	for _, v := range vols {
		kib += v.OutOfSyncKiB
	}
	return kib
}

// roleOf is DRBD's role when it is Primary, else Resyncing while data flows in,
// Secondary with current data, and Stale for any other disk.
func roleOf(role drbd.Role, disk drbd.DiskState, resyncing bool) storage.Role {
	switch {
	case role == drbd.RolePrimary:
		return storage.RolePrimary
	case resyncing:
		return storage.RoleResyncing
	case disk == drbd.DiskUpToDate:
		return storage.RoleSecondary
	}
	return storage.RoleStale
}

// worstDisk is UpToDate only when every volume is, else the first that is not.
func worstDisk[V any](vols []V, disk func(V) drbd.DiskState) drbd.DiskState {
	if len(vols) == 0 {
		return drbd.DiskUnknown
	}
	for _, v := range vols {
		if d := disk(v); d != drbd.DiskUpToDate {
			return d
		}
	}
	return drbd.DiskUpToDate
}

// inState selects the volumes whose replication is in one of the given states.
func inState(vols []drbd.PeerVolume, states ...drbd.Replication) []drbd.PeerVolume {
	var out []drbd.PeerVolume
	for _, v := range vols {
		if slices.Contains(states, v.Replication) {
			out = append(out, v)
		}
	}
	return out
}

// slowest reports the least-complete of the syncs, the one that bounds the replica.
func slowest(vols []drbd.PeerVolume) (percent float64, outOfSyncKiB uint64) {
	for i, v := range vols {
		if i == 0 || v.PercentInSync < percent {
			percent, outOfSyncKiB = v.PercentInSync, v.OutOfSyncKiB
		}
	}
	return percent, outOfSyncKiB
}
