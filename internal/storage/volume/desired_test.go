package volume

import (
	"net/netip"
	"reflect"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage/drbd"
)

func allocation() drbd.Allocation {
	return drbd.Allocation{
		Name: "vol-a1", Minor: 3, Port: 7793, Next: 4,
		NodeIDs: map[string]int{"n2": 1, "n1": 0, "n4": 3},
		Retired: []int{2},
	}
}

func addrs() map[string]netip.Addr {
	return map[string]netip.Addr{
		"n1": netip.MustParseAddr("192.168.1.1"), "n2": netip.MustParseAddr("192.168.1.2"),
		"n4": netip.MustParseAddr("192.168.1.4"),
	}
}

func TestFromAllocationBuildsMembersInNodeIDOrder(t *testing.T) {
	d, err := FromAllocation(allocation(), "n2", 64<<20, true, addrs())
	if err != nil {
		t.Fatal(err)
	}
	want := []drbd.Member{
		{Host: "n1", NodeID: 0, Address: addrs()["n1"]},
		{Host: "n2", NodeID: 1, Address: addrs()["n2"]},
		{Host: "n4", NodeID: 3, Address: addrs()["n4"]},
	}
	if !reflect.DeepEqual(d.Members, want) || d.Self != "n2" || d.Minor != 3 || d.Port != 7793 ||
		!reflect.DeepEqual(d.Retired, []int{2}) || !d.Thin || d.SizeBytes != 64<<20 {
		t.Errorf("got %+v", d)
	}
	if err := d.validate(); err != nil {
		t.Errorf("built state is invalid: %v", err)
	}
}

func TestFromAllocationDoesNotAliasTheAllocation(t *testing.T) {
	al := allocation()
	d, _ := FromAllocation(al, "n1", 1, true, addrs())
	d.Retired[0] = 99
	if al.Retired[0] != 2 {
		t.Error("desired state shares the allocation's retired slice")
	}
}

func TestFromAllocationNeedsSelfAndEveryAddress(t *testing.T) {
	if _, err := FromAllocation(allocation(), "n3", 1, true, addrs()); experrors.KindOf(err) != experrors.KindNotFound {
		t.Errorf("self without a node-id: %v", err)
	}
	a := addrs()
	delete(a, "n4")
	if _, err := FromAllocation(allocation(), "n1", 1, true, a); experrors.KindOf(err) != experrors.KindNotFound {
		t.Errorf("member without an address: %v", err)
	}
}
