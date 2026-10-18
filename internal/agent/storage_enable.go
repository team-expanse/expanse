package agent

import (
	"context"
	"fmt"
	"time"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
)

const (
	storagePassInterval = 5 * time.Second
	extent              = 4 << 20 // LVM's default extent; pool sizes round down to it
)

// storageLoop turns storage on when the web UI asks (storage.EnableRequest) and
// reports where this node's storage stands for the UI and volume placement.
func (a *Agent) storageLoop(ctx context.Context) {
	tick := time.NewTicker(storagePassInterval)
	defer tick.Stop()
	for {
		a.publishStorage(ctx, a.storagePass(ctx))
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// publishStorage writes the report when it changed, and the capacity when the VG did.
func (a *Agent) publishStorage(ctx context.Context, rep storage.NodeStorage) {
	if a.lastStore != nil && *a.lastStore == rep {
		return
	}
	if err := storage.SaveNodeStorage(ctx, a.store, a.cfg.NodeID, rep); err != nil {
		a.logger.Warn("storage report not written", "err", err)
		return
	}
	if rep.State != a.lastState() {
		a.logger.Info("storage", "state", rep.State, "vg", rep.VG, "pool", rep.Pool, "message", rep.Message)
	}
	a.lastStore = &rep
	a.invMu.Lock()
	inv := a.invSnapshot
	a.invMu.Unlock()
	if inv != nil {
		a.publishCapacity(ctx, inv)
	}
}

func (a *Agent) lastState() storage.NodeStorageState {
	if a.lastStore == nil {
		return ""
	}
	return a.lastStore.State
}

// storagePass converges one step toward the requested storage and reports the result.
func (a *Agent) storagePass(ctx context.Context) storage.NodeStorage {
	if a.cfg.StorageVG != "" {
		return a.storageReport(ctx, storage.NodeStorage{Source: storage.SourceConfig, VG: a.cfg.StorageVG, Pool: a.cfg.StoragePool})
	}
	req, ok, err := storage.LoadEnableRequest(store.WithStale(ctx), a.store, a.cfg.NodeID)
	if err != nil {
		return a.withVG(ctx, storage.NodeStorage{State: storage.NodeStorageFailed, VG: storage.DefaultVG, Message: err.Error()})
	}
	if !ok {
		return a.withVG(ctx, storage.NodeStorage{State: storage.NodeStorageOff, VG: storage.DefaultVG})
	}
	req = req.WithDefaults()
	rep := storage.NodeStorage{Source: storage.SourceUI, VG: req.VG, Pool: req.Pool}
	if err := a.ensurePool(ctx, req); err != nil {
		rep.State, rep.Message = storage.NodeStorageFailed, err.Error()
		return a.withVG(ctx, rep)
	}
	rep = a.storageReport(ctx, rep)
	if rep.State == storage.NodeStorageReady {
		a.startVolumes(ctx, func() volumeRunner { return a.newVolNode(req.VG, req.Pool) })
		a.storageVG.Store(req.VG)
	}
	return rep
}

// storageReport is rep with its sizes, ready unless DRBD 9 is not loaded.
func (a *Agent) storageReport(ctx context.Context, rep storage.NodeStorage) storage.NodeStorage {
	rep.State = storage.NodeStorageReady
	if err := a.drbdCheck(); err != nil {
		rep.State, rep.Message = storage.NodeStorageReboot, err.Error()
	}
	if rep.Pool != "" {
		if lv, err := a.lvm.Get(ctx, rep.VG, rep.Pool); err == nil {
			rep.PoolSize = lv.Size
		}
	}
	return a.withVG(ctx, rep)
}

// withVG adds the VG's size and free space, or says why it cannot.
func (a *Agent) withVG(ctx context.Context, rep storage.NodeStorage) storage.NodeStorage {
	vg, err := a.lvm.VG(ctx, rep.VG)
	if err != nil {
		if rep.Message == "" {
			rep.Message = fmt.Sprintf("this node has no volume group %q", rep.VG)
		}
		return rep
	}
	rep.VGSize, rep.VGFree = vg.Size, vg.Free
	return rep
}

// ensurePool creates the thin pool from the requested share of the VG's free space.
func (a *Agent) ensurePool(ctx context.Context, req storage.EnableRequest) error {
	if err := req.Validate(); err != nil {
		return err
	}
	_, err := a.lvm.Get(ctx, req.VG, req.Pool)
	if err == nil {
		return nil
	}
	if experrors.KindOf(err) != experrors.KindNotFound {
		return err
	}
	vg, err := a.lvm.VG(ctx, req.VG)
	if err != nil {
		return fmt.Errorf("this node has no volume group %q", req.VG)
	}
	size := vg.Free / 100 * uint64(req.Percent) / extent * extent
	if size == 0 {
		return fmt.Errorf("volume group %q has no free space", req.VG)
	}
	a.logger.Info("creating the storage thin pool", "vg", req.VG, "pool", req.Pool, "bytes", size)
	if err := a.lvm.CreateThinPool(ctx, req.VG, req.Pool, size); err != nil {
		return fmt.Errorf("creating the thin pool: %w", err)
	}
	return nil
}

// storageVGName is the VG this node reports as disk capacity, "" while storage is off.
func (a *Agent) storageVGName() string {
	vg, _ := a.storageVG.Load().(string)
	return vg
}
