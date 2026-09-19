// Package device is the exvol block-device layer (Phase 06 T09, §4.4):
// the BlockDevice abstraction the device transport serves, plus the
// exvol-backed implementation wired to the T07 primary coordinator.
//
// Device transport decision (documented per the global-conventions
// note, flagged for planner visibility): NBD is the primary transport.
// No maintainable ublk Go binding exists for our Go/kernel target —
// ublksrv is a C library whose Go wrappers are immature cgo bindings,
// exactly the liability the spec says to avoid. The ublk gap is
// deliberate: NBD (kernel client + this package's userspace server) is
// well-understood, needs no kernel module, and `doctor` reports it.
// ublk remains a later optimization if a stable Go binding appears.
//
// §4.4 rules implemented here:
//   - flush must force all quorum replicas to fsync before returning
//     (wired to the coordinator's flush marker — same quorum rule as a
//     write; filesystems depend on this for journaling correctness);
//   - on lease loss the device returns EIO promptly, never hangs —
//     every entry point checks the lease synchronously before doing
//     work, and the coordinator paths are all deadline-bounded. A hung
//     device makes unkillable D-state processes; EIO is strictly better.
package device

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage/exvol/primary"
)

// BlockDevice is the swappable device interface (§4.4). Exactly as
// specified: read, write, flush, discard, size, close.
type BlockDevice interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Flush(ctx context.Context) error
	Discard(off, length int64) error
	Size() int64
	Close() error
}

// LocalReadWriter is the primary's local zvol: the read path (§4.3
// reads are served from the primary's local replica) plus discard.
type LocalReadWriter interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) error
	Flush() error
	Discard(off, length int64) error
	Close() error
}

// ExvolDevice serves one volume from the primary node: reads from the
// local zvol, writes/flushes through the replication coordinator.
type ExvolDevice struct {
	volID string
	size  atomic.Int64 // read on every I/O; SetSize writes from the reconcile goroutine (G6.14)
	local LocalReadWriter
	coord *primary.Coordinator
	lease primary.Lease

	mu     sync.Mutex
	closed bool
}

// New builds the device for one volume.
func New(volID string, size int64, local LocalReadWriter, coord *primary.Coordinator, l primary.Lease) *ExvolDevice {
	d := &ExvolDevice{volID: volID, local: local, coord: coord, lease: l}
	d.size.Store(size)
	return d
}

// leaseErr is the §4.4 lease-loss error: EIO, immediately.
func leaseErr(op string) error {
	return experrors.New(experrors.KindInternal, "exvol.device."+op, "EIO: volume lease lost")
}

func (d *ExvolDevice) check(op string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return experrors.New(experrors.KindInternal, "exvol.device."+op, "device closed")
	}
	if d.lease != nil && !d.lease.Valid() {
		return leaseErr(op)
	}
	return nil
}

// ReadAt serves reads from the primary's local zvol (§4.3 read path).
func (d *ExvolDevice) ReadAt(p []byte, off int64) (int, error) {
	if err := d.check("read"); err != nil {
		return 0, err
	}
	size := d.size.Load()
	if off < 0 || off+int64(len(p)) > size {
		return 0, experrors.New(experrors.KindInvalid, "exvol.device.read",
			fmt.Sprintf("read [%d,%d) out of range (size %d)", off, off+int64(len(p)), size))
	}
	if d.coord != nil {
		// Never return bytes that are not quorum-durable yet.
		if err := d.coord.ReadBarrier(off, int64(len(p))); err != nil {
			return 0, err
		}
	}
	return d.local.ReadAt(p, off)
}

// WriteAt replicates the write (quorum-durable before returning).
func (d *ExvolDevice) WriteAt(p []byte, off int64) (int, error) {
	if err := d.check("write"); err != nil {
		return 0, err
	}
	size := d.size.Load()
	if off < 0 || off+int64(len(p)) > size {
		return 0, experrors.New(experrors.KindInvalid, "exvol.device.write",
			fmt.Sprintf("write [%d,%d) out of range (size %d)", off, off+int64(len(p)), size))
	}
	if d.coord == nil {
		// Test/local-only device: durable local write, no replication.
		return len(p), d.local.WriteAt(p, off)
	}
	if err := d.coord.Write(p, off); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Flush fsyncs every quorum replica via the coordinator's flush marker
// before returning (§4.4 flush-to-quorum).
func (d *ExvolDevice) Flush(ctx context.Context) error {
	if err := d.check("flush"); err != nil {
		return err
	}
	if d.coord == nil {
		return d.local.Flush() // test/local-only device
	}
	return d.coord.Flush()
}

// Discard is a trim hint: punch a hole in the primary's local zvol.
// NOT replicated, so the replicas keep the bytes the primary drops —
// which is why the NBD export does not advertise TRIM (nbdExportFlags).
func (d *ExvolDevice) Discard(off, length int64) error {
	if err := d.check("discard"); err != nil {
		return err
	}
	size := d.size.Load()
	if off < 0 || off+length > size {
		return experrors.New(experrors.KindInvalid, "exvol.device.discard",
			fmt.Sprintf("discard [%d,%d) out of range (size %d)", off, off+length, size))
	}
	return d.local.Discard(off, length)
}

// Size is the volume size in bytes.
func (d *ExvolDevice) Size() int64 { return d.size.Load() }

// SetSize grows the device's advertised/enforced size (G6.14 online
// resize, grow-only). The NBD wire protocol has no live-resize
// primitive; the caller (runtime.applyResize) then tells the kernel the
// new size in place via ResizeNBD (netlink), with no reconnect.
func (d *ExvolDevice) SetSize(n int64) { d.size.Store(n) }

// Close releases the device (idempotent; subsequent ops error).
func (d *ExvolDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	return d.local.Close()
}
