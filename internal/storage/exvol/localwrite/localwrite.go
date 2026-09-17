// Package localwrite is the local-write half of both the primary and
// secondary replica paths (Phase 06 T06, §4.3 steps 4 and 6c): open a
// zvol block device with O_DIRECT|O_DSYNC, write at the given offset,
// and complete before the replica is counted durable.
//
// Durability layers, both required (spec §4.5: "don't rely on just
// one"): the zvol carries sync=always at the ZFS level AND every write
// here goes out with O_DSYNC at the syscall level. Reads never block on
// either — they are served from the primary's local zvol (§4.3 read
// path).
//
// Alignment requirement (documented per the card): O_DIRECT requires
// the file offset, the byte count, AND the memory buffer address to be
// aligned to the device's logical block size — 4096 here (zvols are
// created with volblocksize=16k, ashift >= 12). Arbitrary offsets and
// lengths from the replication protocol are handled by splitting every
// write into head / middle / tail segments (computeSegments): partial
// edge blocks are read-modify-written through aligned bounce buffers,
// full middle blocks are copied into an aligned buffer and written in
// one pread. The copy is unavoidable — O_DIRECT also constrains the
// memory address, and Go can't hand a syscall an arbitrary []byte.
package localwrite

import (
	"fmt"
	"io"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Align is the logical block size all O_DIRECT I/O must be aligned to.
const Align = 4096

// Writer performs durable unaligned writes to a local block device.
// Safe for concurrent use (a mutex serializes the RMW sequences —
// ordering across concurrent overlapping writes is the protocol layer's
// job, R4).
type Writer struct {
	mu     sync.Mutex
	f      *os.File
	size   int64
	direct bool // O_DIRECT active (false = buffered fallback, tests only)
}

// Open opens the block device for durable direct I/O. Fails if the
// device or the filesystem backing it rejects O_DIRECT.
func Open(path string, size int64) (*Writer, error) {
	return openFlags(path, size, os.O_RDWR|os.O_CREATE|unix.O_DIRECT|unix.O_DSYNC, true)
}

// OpenBuffered opens without O_DIRECT (O_DSYNC only) — for unit tests
// on filesystems that reject O_DIRECT (e.g. tmpfs) and as an escape
// hatch. Production never uses this path.
func OpenBuffered(path string, size int64) (*Writer, error) {
	return openFlags(path, size, os.O_RDWR|os.O_CREATE|unix.O_DSYNC, false)
}

func openFlags(path string, size int64, flags int, direct bool) (*Writer, error) {
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s (direct=%v): %w", path, direct, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	actual := st.Size()
	if size > 0 {
		actual = size
	}
	return &Writer{f: f, size: actual, direct: direct}, nil
}

// Direct reports whether O_DIRECT is active.
func (w *Writer) Direct() bool { return w.direct }

// Size is the device size in bytes.
func (w *Writer) Size() int64 { return w.size }

// Close closes the device.
func (w *Writer) Close() error { return w.f.Close() }

// segment is one piece of an I/O split on Align boundaries.
type segment struct {
	fileOff int64  // aligned file offset
	bufOff  int    // offset within the bounce buffer holding the payload
	n       int    // length to transfer
	rmw     bool   // true = read-modify-write (partial block)
	data    []byte // payload slice for this segment
}

// computeSegments splits [off, off+n) on Align boundaries. Pure — fully
// unit-testable without any device.
func computeSegments(off int64, n int) []segment {
	if n == 0 {
		return nil
	}
	var segs []segment
	end := off + int64(n)

	start := off
	if a := start % Align; a != 0 {
		// Head: first partial block, RMW.
		blockedDown := start - a
		length := int(Align - a)
		if int64(length) > end-start {
			length = int(end - start)
		}
		segs = append(segs, segment{fileOff: blockedDown, bufOff: int(a), n: length, rmw: true})
		start = blockedDown + Align
	}

	// Middle: full aligned blocks, one contiguous transfer.
	if mid := end - start; mid >= Align {
		full := (mid / Align) * Align
		segs = append(segs, segment{fileOff: start, n: int(full)})
		start += full
	}

	if start < end {
		// Tail: last partial block, RMW.
		blockedDown := start - start%Align
		segs = append(segs, segment{
			fileOff: blockedDown, bufOff: int(start % Align),
			n: int(end - start), rmw: true,
		})
	}
	return segs
}

// alignedBuf allocates a slice whose first element is Align-aligned.
func alignedBuf(n int) []byte {
	b := make([]byte, n+Align)
	a := Align - int(uintptr(unsafe.Pointer(&b[0]))%Align)
	return b[a : a+n]
}

// WriteAt writes p at off durably (O_DSYNC on each pwrite): partial
// edge blocks are read-modify-written through aligned bounce buffers,
// middle blocks go out in one aligned transfer. Returns only after the
// data is on stable storage.
func (w *Writer) WriteAt(p []byte, off int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if off < 0 || off+int64(len(p)) > w.size {
		return fmt.Errorf("write [%d,%d) out of range (size %d)", off, off+int64(len(p)), w.size)
	}

	for _, seg := range computeSegments(off, len(p)) {
		var buf []byte
		var err error
		if seg.rmw || w.direct {
			// O_DIRECT always needs an aligned buffer (memory
			// alignment); buffered mode writes p directly when the
			// segment is a full aligned middle block.
			segLen := seg.n
			if seg.rmw {
				segLen = Align
			}
			buf = alignedBuf(segLen)
		}
		switch {
		case seg.rmw:
			// Read the aligned block, patch the payload in, write back.
			if _, err := w.f.ReadAt(buf, seg.fileOff); err != nil && err != io.EOF {
				return fmt.Errorf("rmw read at %d: %w", seg.fileOff, err)
			}
			// Short/EOF reads (fresh zeroed zvol tail) leave the rest
			// zero — alignedBuf starts zeroed.
			copy(buf[seg.bufOff:], segData(p, off, seg))
			_, err = w.f.WriteAt(buf, seg.fileOff)
		case w.direct:
			copy(buf, segData(p, off, seg))
			_, err = w.f.WriteAt(buf, seg.fileOff)
		default:
			_, err = w.f.WriteAt(segData(p, off, seg), seg.fileOff)
		}
		if err != nil {
			return fmt.Errorf("direct write at %d: %w", seg.fileOff, err)
		}
	}
	return nil
}

// ReadAt reads at an arbitrary (unaligned) offset via aligned bounce
// buffers. Used for verification and as the resync read path.
func (w *Writer) ReadAt(p []byte, off int64) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if off < 0 || off+int64(len(p)) > w.size {
		return 0, fmt.Errorf("read [%d,%d) out of range (size %d)", off, off+int64(len(p)), w.size)
	}
	read := 0
	for _, seg := range computeSegments(off, len(p)) {
		if !seg.rmw && !w.direct {
			n, err := w.f.ReadAt(segData(p, off, seg), seg.fileOff)
			read += n
			if err != nil {
				return read, err
			}
			continue
		}
		segLen := seg.n
		if seg.rmw {
			segLen = Align
		}
		buf := alignedBuf(segLen)
		if _, err := w.f.ReadAt(buf, seg.fileOff); err != nil && err != io.EOF {
			return read, err
		}
		// Short reads (EOF at device end) leave the remainder zero —
		// alignedBuf starts zeroed.
		copy(segData(p, off, seg), buf[seg.bufOff:seg.bufOff+seg.n])
		read += seg.n
	}
	return read, nil
}

// Flush fsyncs the device. With O_DSYNC on every write this is a
// no-op in practice, but flush (§4.4) must exist for the device layer.
func (w *Writer) Flush() error { return w.f.Sync() }

// segData returns the slice of the caller's payload for this segment.
func segData(p []byte, off int64, seg segment) []byte {
	// seg.bufOff is relative to the aligned block start; the payload
	// position within p is (segment file start + bufOff) - off.
	start := seg.fileOff + int64(seg.bufOff) - off
	return p[start : start+int64(seg.n)]
}
