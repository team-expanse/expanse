package agent

import (
	"context"

	"github.com/expanse/expanse/internal/agent/inventory"
	"github.com/expanse/expanse/internal/blocks/wire"
	"github.com/expanse/expanse/internal/quantity"
	"github.com/expanse/expanse/internal/storage/lvm"
)

// capacityFrom sizes the node for placement: hardware threads, total memory and the data VG.
func capacityFrom(inv *inventory.Inventory, vgSize uint64) wire.Capacity {
	threads := inv.CPU.Threads
	if threads == 0 {
		threads = inv.CPU.Cores
	}
	return wire.Capacity{
		CPU:  quantity.CPU{Milli: int64(threads) * 1000},
		Mem:  quantity.Bytes{N: int64(inv.Memory.Total)},
		Disk: quantity.Bytes{N: int64(vgSize)},
	}
}

// storageVGSize is the data VG's size, or 0 when the node has none or vgs fails.
func (a *Agent) storageVGSize(ctx context.Context) uint64 {
	if a.cfg.StorageVG == "" {
		return 0
	}
	vg, err := lvm.New().VG(ctx, a.cfg.StorageVG)
	if err != nil {
		a.logger.Warn("storage VG size unavailable; placement keeps the default disk capacity", "vg", a.cfg.StorageVG, "err", err)
		return 0
	}
	return vg.Size
}

func (a *Agent) publishCapacity(ctx context.Context, inv *inventory.Inventory) {
	c := wire.FormatCapacity(capacityFrom(inv, a.storageVGSize(ctx)))
	if _, err := a.store.Put(ctx, wire.CapacityKey(a.cfg.NodeID), []byte(c)); err != nil {
		a.logger.Error("write node capacity failed", "err", err)
	}
}
