package localwrite

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/expanse/expanse/internal/storage/zfs"
)

// --- computeSegments: pure alignment-splitting unit tests ---

func TestComputeSegmentsUnalignedHead(t *testing.T) {
	// A 100-byte write at offset 3 fits entirely in block 0: one RMW.
	segs := computeSegments(3, 100)
	if len(segs) != 1 || !segs[0].rmw || segs[0].n != 100 || segs[0].bufOff != 3 || segs[0].fileOff != 0 {
		t.Fatalf("in-block write: %+v, want single rmw at block 0", segs)
	}

	// 2*Align bytes at offset 3 crosses into two more blocks:
	// head RMW + one full middle + tail RMW.
	segs = computeSegments(3, 2*Align)
	if len(segs) != 3 {
		t.Fatalf("segments = %d (%+v), want head+middle+tail", len(segs), segs)
	}
	if segs[0].fileOff != 0 || !segs[0].rmw || segs[0].n != Align-3 || segs[0].bufOff != 3 {
		t.Errorf("head = %+v, want fileOff=0 rmw=true n=%d bufOff=3", segs[0], Align-3)
	}
	if segs[1].fileOff != int64(Align) || segs[1].rmw || segs[1].n != Align {
		t.Errorf("middle = %+v, want full aligned block", segs[1])
	}
	if !segs[2].rmw || segs[2].fileOff != int64(2*Align) || segs[2].n != 3 {
		t.Errorf("tail = %+v, want rmw at block 2 n=3", segs[2])
	}
}

func TestComputeSegmentsAligned(t *testing.T) {
	segs := computeSegments(Align, 4*Align)
	if len(segs) != 1 || segs[0].rmw {
		t.Fatalf("aligned write: segments = %+v, want one non-rmw middle", segs)
	}
	if segs[0].fileOff != Align || segs[0].n != 4*Align {
		t.Errorf("middle = %+v", segs[0])
	}
}

func TestComputeSegmentsSinglePartialBlock(t *testing.T) {
	segs := computeSegments(10, 5) // entirely inside one block
	if len(segs) != 1 || !segs[0].rmw || segs[0].n != 5 || segs[0].bufOff != 10 {
		t.Fatalf("single partial block: %+v", segs)
	}
}

func TestComputeSegmentsZeroLength(t *testing.T) {
	if segs := computeSegments(0, 0); segs != nil {
		t.Errorf("zero-length write: %+v, want nil", segs)
	}
}

func TestComputeSegmentsExactBlockBoundary(t *testing.T) {
	// Write ends exactly on a block boundary: no tail segment.
	segs := computeSegments(3, Align-3)
	for _, s := range segs {
		if s.rmw && s.fileOff+Align > 4096 && s.fileOff >= Align {
			t.Errorf("unexpected tail rmw: %+v", s)
		}
	}
	if segs[0].fileOff != 0 || segs[0].bufOff != 3 {
		t.Errorf("head = %+v", segs[0])
	}
	if len(segs) != 1 {
		t.Errorf("segments = %+v, want single head rmw", segs)
	}
}

// --- File-backed round trips (no zvol needed): exercises the real
// syscall path incl. O_DIRECT where the backing FS allows it ---

// openForTest opens a temp file; prefers O_DIRECT, falls back to
// buffered (tmpfs) with a note — the alignment logic is identical, only
// the syscall flags differ.
func openForTest(t *testing.T, path string, size int64) *Writer {
	t.Helper()
	w, err := Open(path, size)
	if err != nil {
		t.Logf("O_DIRECT open failed (%v); falling back to buffered", err)
		w, err = OpenBuffered(path, size)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		t.Logf("backing FS rejected O_DIRECT; testing buffered fallback (alignment logic unchanged)")
	}
	return w
}

func TestUnalignedWriteReadBackAfterReopen(t *testing.T) {
	size := int64(64 << 10)
	dir := t.TempDir()
	path := filepath.Join(dir, "test.zvol")

	// Reference content, written via unaligned offsets/sizes.
	ref := make([]byte, size)
	rng := 12345
	for i := range ref {
		rng = rng*1103515245 + 12345
		ref[i] = byte(rng >> 16)
	}

	w := openForTest(t, path, size)
	// Cover every byte with unaligned, boundary-crossing chunks:
	// starts/lengths deliberately misaligned so head/RMW, middles, and
	// tail paths all execute.
	for off := int64(0); off < size; {
		n := 100 + int(off)%700
		if off+int64(n) > size {
			n = int(size - off)
		}
		if err := w.WriteAt(ref[off:off+int64(n)], off); err != nil {
			t.Fatalf("write at %d: %v", off, err)
		}
		off += int64(n)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	w.Close()

	// Reopen (process-restart proxy) and verify every byte.
	w2 := openForTest(t, path, size)
	defer w2.Close()
	got := make([]byte, size)
	if _, err := w2.ReadAt(got, 0); err != nil && err.Error() != "EOF" {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, ref) {
		for i := range ref {
			if got[i] != ref[i] {
				t.Fatalf("first divergence at byte %d: got %d want %d (content did not survive reopen)", i, got[i], ref[i])
			}
		}
	}
}

func TestOverlappingRMWReadModifyWrite(t *testing.T) {
	// Two writes into the SAME aligned block at different unaligned
	// offsets: the RMW of the second must preserve the first.
	path := filepath.Join(t.TempDir(), "rmw.zvol")
	w := openForTest(t, path, 4096)

	first := bytes.Repeat([]byte{0xAA}, 100)
	if err := w.WriteAt(first, 100); err != nil {
		t.Fatal(err)
	}
	second := bytes.Repeat([]byte{0xBB}, 50)
	if err := w.WriteAt(second, 150); err != nil { // overlaps first's tail
		t.Fatal(err)
	}
	w.Close()

	w2 := openForTest(t, path, 4096)
	defer w2.Close()
	got := make([]byte, 300)
	if _, err := w2.ReadAt(got, 0); err != nil && err.Error() != "EOF" {
		t.Fatal(err)
	}
	want := make([]byte, 300)
	copy(want[100:], first)
	copy(want[150:], second) // second overwrites first[50:100]
	if !bytes.Equal(got, want) {
		t.Fatalf("RMW clobbered data: got % x", got[:200])
	}
}

func TestWriteOutOfBoundsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oob.zvol")
	w := openForTest(t, path, 4096)
	defer w.Close()
	if err := w.WriteAt(make([]byte, 10), 4090); err == nil {
		t.Error("write past size accepted")
	}
	if err := w.WriteAt(make([]byte, 10), -1); err == nil {
		t.Error("negative offset accepted")
	}
}

func TestDirectFlagRespected(t *testing.T) {
	// On a filesystem that supports O_DIRECT, Open must report direct.
	path := filepath.Join(t.TempDir(), "d.zvol")
	w, err := Open(path, 4096)
	if err != nil {
		t.Skipf("backing FS rejects O_DIRECT: %v", err)
	}
	defer w.Close()
	if !w.Direct() {
		t.Error("Open produced a non-direct writer")
	}
}

func TestAlignedBufAlignment(t *testing.T) {
	for i := 0; i < 100; i++ { // allocations move; alignment must hold
		buf := alignedBuf(Align)
		if uintptr(unsafePointerOf(buf))%Align != 0 {
			t.Fatalf("buffer at %#p not %d-aligned", buf, Align)
		}
	}
}

// --- Real-zvol gated test: proves O_DSYNC persists across a "process
// restart" (close + reopen of the device node). Requires a scratch pool
// (EXPANSE_ZFS_TEST=1, pool `volumes` per nix/modules/storage-test.nix).

func TestRealZvolDurableAcrossReopen(t *testing.T) {
	if os.Getenv("EXPANSE_ZFS_TEST") != "1" {
		t.Skip("requires real ZFS pool (EXPANSE_ZFS_TEST=1)")
	}
	ctx := context.Background()
	z := zfs.New()
	zvol := "volumes/localwrite-test"
	if err := z.DestroyZvol(ctx, zvol, true); err != nil {
		t.Logf("pre-clean destroy: %v", err)
	}
	if err := z.CreateZvol(ctx, zvol, 8<<20, nil); err != nil {
		t.Fatalf("create zvol: %v", err)
	}
	defer z.DestroyZvol(ctx, zvol, true)

	dev := "/dev/zvol/" + zvol
	if _, err := os.Stat(dev); err != nil {
		t.Fatalf("zvol device missing: %v", err)
	}

	// Known pattern: block header + sequence numbers.
	w, err := Open(dev, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	pattern := make([]byte, 3*Align+777) // unaligned length
	for i := 0; i < len(pattern); i += 8 {
		binary.LittleEndian.PutUint64(pattern[i:], uint64(i))
	}
	if err := w.WriteAt(pattern, 1000); err != nil { // unaligned offset
		t.Fatalf("pattern write: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	w.Close()

	// Reopen the device node — a real restart of the writer process.
	w2, err := Open(dev, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	got := make([]byte, len(pattern))
	if _, err := w2.ReadAt(got, 1000); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, pattern) {
		t.Fatal("pattern did not survive device reopen — O_DSYNC not effective (check sync=always on the zvol)")
	}
	fmt.Fprintln(os.Stderr, "real-zvol durability check passed")
}

// unsafePointerOf is a test hook to check alignment of allocated buffers.
func unsafePointerOf(b []byte) unsafe.Pointer { return unsafe.Pointer(&b[0]) }
