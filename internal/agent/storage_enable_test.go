package agent

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/storage/lvm"
	"github.com/expanse/expanse/internal/store/boltstore"
)

// fakeLVM has one VG and records the thin pool it is asked to create.
type fakeLVM struct {
	lvm.LVM
	vg       lvm.VG
	pool     *lvm.LV
	createFn func(size uint64) error
}

func (f *fakeLVM) VG(_ context.Context, name string) (lvm.VG, error) {
	if name != f.vg.Name {
		return lvm.VG{}, experrors.New(experrors.KindNotFound, "fake", "Volume group \""+name+"\" not found")
	}
	return f.vg, nil
}

func (f *fakeLVM) Get(_ context.Context, vg, name string) (lvm.LV, error) {
	if f.pool == nil || f.pool.Name != name {
		return lvm.LV{}, experrors.New(experrors.KindNotFound, "fake", "Failed to find logical volume")
	}
	return *f.pool, nil
}

func (f *fakeLVM) CreateThinPool(_ context.Context, vg, pool string, size uint64) error {
	if f.createFn != nil {
		if err := f.createFn(size); err != nil {
			return err
		}
	}
	f.pool = &lvm.LV{Name: pool, VG: vg, Size: size, Type: lvm.ThinPool}
	f.vg.Free -= size
	return nil
}

type fakeVolNode struct{ started chan string }

func (f *fakeVolNode) Run(ctx context.Context, _ time.Duration) { <-ctx.Done() }

func newStorageAgent(t *testing.T) (*Agent, *fakeLVM, *fakeVolNode) {
	t.Helper()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := newTestAgent(t, st)
	a.cfg.NodeID = "n1"
	fl := &fakeLVM{vg: lvm.VG{Name: "expanse", Size: 100 << 30, Free: 100 << 30}}
	node := &fakeVolNode{started: make(chan string, 1)}
	a.lvm = fl
	a.drbdCheck = func() error { return nil }
	a.newVolNode = func(vg, pool string) volumeRunner {
		node.started <- vg + "/" + pool
		return node
	}
	t.Cleanup(a.stopVolumes)
	return a, fl, node
}

func TestStoragePassReportsOffWithoutARequest(t *testing.T) {
	a, fl, node := newStorageAgent(t)
	got := a.storagePass(context.Background())
	if got.State != storage.NodeStorageOff || got.VG != "expanse" || got.VGFree != 100<<30 {
		t.Fatalf("report = %+v", got)
	}
	if fl.pool != nil || len(node.started) != 0 {
		t.Fatal("built storage nobody asked for")
	}
}

func TestStoragePassReportsAMissingVG(t *testing.T) {
	a, fl, _ := newStorageAgent(t)
	fl.vg.Name = "other"
	got := a.storagePass(context.Background())
	if got.State != storage.NodeStorageOff || got.Message == "" {
		t.Fatalf("report = %+v, want off with a reason", got)
	}
}

func TestStoragePassBuildsThePoolAndStartsVolumes(t *testing.T) {
	ctx := context.Background()
	a, fl, node := newStorageAgent(t)
	if err := storage.SaveEnableRequest(ctx, a.store, "n1", storage.EnableRequest{}.WithDefaults()); err != nil {
		t.Fatal(err)
	}
	got := a.storagePass(ctx)
	if got.State != storage.NodeStorageReady || got.Source != storage.SourceUI || got.Pool != "pool" {
		t.Fatalf("report = %+v", got)
	}
	if fl.pool == nil || fl.pool.Size > 95<<30 || fl.pool.Size < 94<<30 || fl.pool.Size%(4<<20) != 0 {
		t.Fatalf("pool = %+v, want 95%% of the free space in whole 4 MiB extents", fl.pool)
	}
	if got.PoolSize != fl.pool.Size {
		t.Errorf("pool size reported %d, want %d", got.PoolSize, fl.pool.Size)
	}
	if s := <-node.started; s != "expanse/pool" {
		t.Errorf("volume node for %s", s)
	}
	// A second pass neither rebuilds the pool nor starts a second node.
	size := fl.pool.Size
	if again := a.storagePass(ctx); again.State != storage.NodeStorageReady || fl.pool.Size != size || len(node.started) != 0 {
		t.Fatalf("second pass: %+v", again)
	}
	if a.storageVGName() != "expanse" {
		t.Errorf("capacity VG = %q", a.storageVGName())
	}
}

func TestStoragePassAsksForARebootUnderDRBD84(t *testing.T) {
	ctx := context.Background()
	a, fl, node := newStorageAgent(t)
	a.drbdCheck = func() error { return errors.New("DRBD 8.4.11 is loaded but volumes need DRBD 9") }
	_ = storage.SaveEnableRequest(ctx, a.store, "n1", storage.EnableRequest{}.WithDefaults())
	got := a.storagePass(ctx)
	if got.State != storage.NodeStorageReboot || got.Message == "" {
		t.Fatalf("report = %+v", got)
	}
	if fl.pool == nil {
		t.Error("the pool can be built before the reboot")
	}
	if len(node.started) != 0 {
		t.Error("volumes started on DRBD 8.4")
	}
}

func TestStoragePassReportsAPoolItCannotBuild(t *testing.T) {
	ctx := context.Background()
	a, fl, node := newStorageAgent(t)
	fl.createFn = func(uint64) error { return errors.New("Insufficient free space") }
	_ = storage.SaveEnableRequest(ctx, a.store, "n1", storage.EnableRequest{}.WithDefaults())
	got := a.storagePass(ctx)
	if got.State != storage.NodeStorageFailed || got.Message == "" {
		t.Fatalf("report = %+v", got)
	}
	if len(node.started) != 0 {
		t.Error("volumes started without a pool")
	}
}

func TestStoragePassReportsTheNodeConfig(t *testing.T) {
	ctx := context.Background()
	a, fl, node := newStorageAgent(t)
	a.cfg.StorageVG, a.cfg.StoragePool = "expanse", "pool"
	fl.pool = &lvm.LV{Name: "pool", VG: "expanse", Size: 50 << 30, Type: lvm.ThinPool}
	got := a.storagePass(ctx)
	if got.State != storage.NodeStorageReady || got.Source != storage.SourceConfig || got.PoolSize != 50<<30 {
		t.Fatalf("report = %+v", got)
	}
	if len(node.started) != 0 {
		t.Error("the config's volume node is started by New, not by the pass")
	}
}

func TestStoppedVolumesNeverStartAgain(t *testing.T) {
	ctx := context.Background()
	a, _, node := newStorageAgent(t)
	a.stopVolumes()
	_ = storage.SaveEnableRequest(ctx, a.store, "n1", storage.EnableRequest{}.WithDefaults())
	a.storagePass(ctx)
	if len(node.started) != 0 {
		t.Fatal("volumes started during shutdown")
	}
}
