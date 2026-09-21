package volume

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCopyDeviceWritesExactlyTheFirstNBytesAndLeavesTheRest(t *testing.T) {
	src := writeFile(t, "src", bytes.Repeat([]byte("S"), 3*copyChunk))
	dst := writeFile(t, "dst", bytes.Repeat([]byte("D"), 4*copyChunk))
	n := uint64(2*copyChunk + 17)
	if err := copyDevice(context.Background(), src, dst, n); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	want := append(bytes.Repeat([]byte("S"), int(n)), bytes.Repeat([]byte("D"), 4*copyChunk-int(n))...)
	if !bytes.Equal(got, want) {
		t.Error("dst is not the source's first n bytes followed by its own tail")
	}
}

func TestCopyDeviceStopsWhenItsContextEnds(t *testing.T) {
	src := writeFile(t, "src", bytes.Repeat([]byte("S"), 3*copyChunk))
	dst := writeFile(t, "dst", make([]byte, 3*copyChunk))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyDevice(ctx, src, dst, 3*copyChunk); err == nil {
		t.Error("a cancelled copy reported success")
	}
}

func TestCopyDeviceFailsOnAMissingSourceOrTarget(t *testing.T) {
	file := writeFile(t, "f", []byte("x"))
	missing := filepath.Join(t.TempDir(), "nope")
	if err := copyDevice(context.Background(), missing, file, 1); err == nil {
		t.Error("missing source accepted")
	}
	if err := copyDevice(context.Background(), file, missing, 1); err == nil {
		t.Error("missing target accepted: it must never be created, only opened")
	}
}
