package vm

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// sparseFile makes a file of size with data written at each offset.
func sparseFile(t *testing.T, size int64, data map[int64][]byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	for off, b := range data {
		if _, err := f.WriteAt(b, off); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func qcow2Header(size uint64, backing uint64, incompat uint64) []byte {
	h := make([]byte, 104)
	copy(h, "QFI\xfb")
	binary.BigEndian.PutUint32(h[4:], 3)
	binary.BigEndian.PutUint64(h[8:], backing)
	binary.BigEndian.PutUint64(h[24:], size)
	binary.BigEndian.PutUint64(h[72:], incompat)
	return h
}

func TestProbe(t *testing.T) {
	q := sparseFile(t, 4096, map[int64][]byte{0: qcow2Header(20<<30, 0, 0)})
	if f, size, err := probe(q); err != nil || f != "qcow2" || size != 20<<30 {
		t.Fatalf("qcow2: %s %d %v", f, size, err)
	}
	r := sparseFile(t, 3<<20, map[int64][]byte{0: []byte("\xeb\x63\x90")})
	if f, size, err := probe(r); err != nil || f != "raw" || size != 3<<20 {
		t.Fatalf("raw: %s %d %v", f, size, err)
	}
	for name, h := range map[string][]byte{
		"backing file":  qcow2Header(1<<30, 512, 0),
		"external data": qcow2Header(1<<30, 0, 1<<2),
	} {
		if _, _, err := probe(sparseFile(t, 4096, map[int64][]byte{0: h})); err == nil {
			t.Errorf("%s: accepted; it would read host files into the guest", name)
		}
	}
}

func TestBlank(t *testing.T) {
	if ok, err := Blank(sparseFile(t, 8<<20, nil)); err != nil || !ok {
		t.Fatalf("zeroed disk: %v, %v", ok, err)
	}
	if ok, _ := Blank(sparseFile(t, 8<<20, map[int64][]byte{headerSize - 1: {1}})); ok {
		t.Error("a disk with a byte in its first MiB is blank")
	}
	if ok, _ := Blank(sparseFile(t, 8<<20, map[int64][]byte{headerSize: {1}})); !ok {
		t.Error("only the first MiB decides")
	}
}

func TestWriteRawCopiesDataAndWritesTheHeaderLast(t *testing.T) {
	src := sparseFile(t, 6<<20, map[int64][]byte{
		0:       []byte("boot sector"),
		5 << 20: []byte("root fs data"),
	})
	dev := sparseFile(t, 16<<20, nil)
	var order []int64
	if err := writeRaw(src, dev, func(off int64) { order = append(order, off) }); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dev)
	if !bytes.HasPrefix(got, []byte("boot sector")) || !bytes.Equal(got[5<<20:5<<20+12], []byte("root fs data")) {
		t.Fatal("the image is not on the disk")
	}
	if len(order) == 0 || order[len(order)-1] != 0 {
		t.Fatalf("write order %v: the first MiB must go last, so a cut-short write leaves the disk blank", order)
	}
	if ok, _ := Blank(dev); ok {
		t.Error("written disk still blank")
	}
}

func TestWriteRawRefusesASmallDisk(t *testing.T) {
	src := sparseFile(t, 8<<20, nil)
	dev := sparseFile(t, 4<<20, nil)
	if err := writeRaw(src, dev, nil); err == nil {
		t.Fatal("wrote an 8 MiB image onto a 4 MiB disk")
	}
}
