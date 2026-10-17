package agent

import (
	"testing"

	"github.com/expanse/expanse/internal/agent/inventory"
	"github.com/expanse/expanse/internal/blocks/wire"
)

// The R630: 32 threads, 378 GiB, a 1.49 TB data VG. Placement counts threads, as qemu's -smp does.
func TestCapacityFromInventory(t *testing.T) {
	inv := &inventory.Inventory{
		CPU:    inventory.CPUInfo{Cores: 16, Threads: 32},
		Memory: inventory.MemoryInfo{Total: 405874409472},
	}
	got := capacityFrom(inv, 1599784443904)
	if got.CPU.Milli != 32000 || got.Mem.N != 405874409472 || got.Disk.N != 1599784443904 {
		t.Errorf("capacityFrom = %+v", got)
	}
	if s := wire.FormatCapacity(got); s != "cpu=32000 memory=405874409472 disk=1599784443904" {
		t.Errorf("FormatCapacity = %q", s)
	}
}

// No storage VG (or vgs failed) leaves disk out, so the scheduler keeps its default for it.
func TestCapacityWithoutVG(t *testing.T) {
	inv := &inventory.Inventory{CPU: inventory.CPUInfo{Cores: 2}, Memory: inventory.MemoryInfo{Total: 4 << 30}}
	got := capacityFrom(inv, 0)
	if s := wire.FormatCapacity(got); s != "cpu=2000 memory=4294967296" {
		t.Errorf("FormatCapacity = %q", s)
	}
}
