// Package volume is the node-side runtime that makes one machine's LVM and DRBD
// state match the desired placement of a volume. It moves no data: DRBD does.
package volume

import (
	"fmt"
	"net/netip"
	"slices"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage/drbd"
)

// Desired is everything one node needs to converge a volume, as plain data.
type Desired struct {
	Name      string
	SizeBytes uint64 // usable size; the backing LV is larger by DRBD's metadata
	Thin      bool
	Minor     int
	Port      int
	Self      string
	Members   []drbd.Member // live replicas and any tiebreaker, including Self
	// Retired are node-ids of dropped replicas that this node must forget.
	Retired []int
}

// FromAllocation builds the desired state for self from the cluster allocation.
// addrs maps each member host to its mesh address.
func FromAllocation(al drbd.Allocation, self string, size uint64, thin bool, addrs map[string]netip.Addr) (Desired, error) {
	const op = "volume.FromAllocation"
	_, replica := al.NodeIDs[self]
	_, tiebreaker := al.Diskless[self]
	if !replica && !tiebreaker {
		return Desired{}, experrors.New(experrors.KindNotFound, op, fmt.Sprintf("%q has no node-id in %q", self, al.Name))
	}
	ms := make([]drbd.Member, 0, len(al.NodeIDs)+len(al.Diskless))
	for _, group := range []struct {
		ids      map[string]int
		diskless bool
	}{{al.NodeIDs, false}, {al.Diskless, true}} {
		for host, id := range group.ids {
			addr, ok := addrs[host]
			if !ok {
				return Desired{}, experrors.New(experrors.KindNotFound, op, fmt.Sprintf("no address for member %q of %q", host, al.Name))
			}
			ms = append(ms, drbd.Member{Host: host, NodeID: id, Address: addr, Diskless: group.diskless})
		}
	}
	slices.SortFunc(ms, func(a, b drbd.Member) int { return a.NodeID - b.NodeID })
	d := Desired{Name: al.Name, SizeBytes: size, Thin: thin, Minor: al.Minor, Port: al.Port, Self: self, Members: ms}
	if replica {
		d.Retired = slices.Clone(al.Retired) // a tiebreaker keeps no bitmap slots to forget
	}
	return d, nil
}

// Tiebreaker reports whether this node only votes in quorum and holds none of the data.
func (d Desired) Tiebreaker() bool {
	return slices.ContainsFunc(d.Members, func(m drbd.Member) bool { return m.Host == d.Self && m.Diskless })
}

func (d Desired) validate() error {
	invalid := func(format string, args ...any) error {
		return experrors.New(experrors.KindInvalid, "volume.Reconcile", fmt.Sprintf("volume %q: "+format, append([]any{d.Name}, args...)...))
	}
	live := map[int]bool{}
	self := false
	for _, m := range d.Members {
		live[m.NodeID] = true
		self = self || m.Host == d.Self
	}
	switch {
	case d.SizeBytes == 0:
		return invalid("size must be positive")
	case !self:
		return invalid("self %q is not a member", d.Self)
	}
	for _, id := range d.Retired {
		switch {
		case id < 0 || id > drbd.MaxNodeID:
			return invalid("retired node-id %d out of range", id)
		case live[id]:
			return invalid("node-id %d is both live and retired", id)
		}
	}
	return nil
}
