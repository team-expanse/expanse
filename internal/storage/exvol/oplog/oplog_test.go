package oplog

import (
	"path/filepath"
	"testing"
)

func TestOpenLoadsExistingRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vol.oplog")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	st.Append(1, Record{Offset: 0, Length: 4096, CRC: 111}, false)
	st.Append(2, Record{Offset: 4096, Length: 4096, CRC: 222}, false)
	st.Append(3, Record{Offset: 0, Length: 0, CRC: 0}, true) // flush marker

	if got := st.MaxSeq(); got != 3 {
		t.Fatalf("MaxSeq before reopen = %d, want 3", got)
	}

	// Simulate a restart: a fresh Store opened at the same path must see
	// every record the previous process appended.
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := reopened.MaxSeq(); got != 3 {
		t.Fatalf("MaxSeq after reopen = %d, want 3", got)
	}
	rec, ok := reopened.Get(2)
	if !ok || rec.Offset != 4096 || rec.Length != 4096 || rec.CRC != 222 {
		t.Fatalf("Get(2) after reopen = %+v, ok=%v", rec, ok)
	}
	flush, ok := reopened.Get(3)
	if !ok || flush.Length != 0 {
		t.Fatalf("Get(3) (flush) after reopen = %+v, ok=%v", flush, ok)
	}

	// A third process must be able to keep appending past what the
	// second process wrote (role handoff: primary writes, then a
	// restart rejoins as secondary and keeps receiving).
	reopened.Append(4, Record{Offset: 8192, Length: 4096, CRC: 333}, false)
	third, err := Open(path)
	if err != nil {
		t.Fatalf("third open: %v", err)
	}
	if got := third.MaxSeq(); got != 4 {
		t.Fatalf("MaxSeq after third open = %d, want 4", got)
	}
}

func TestResetClearsRecordsAndFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vol.oplog")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	st.Append(1, Record{Offset: 0, Length: 4096, CRC: 1}, false)
	st.Reset()
	if got := st.MaxSeq(); got != 0 {
		t.Fatalf("MaxSeq after Reset = %d, want 0", got)
	}
	if len(st.Snapshot()) != 0 {
		t.Fatalf("Snapshot after Reset not empty: %v", st.Snapshot())
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after reset: %v", err)
	}
	if got := reopened.MaxSeq(); got != 0 {
		t.Fatalf("MaxSeq after reopen post-reset = %d, want 0 (truncated file)", got)
	}
}

func TestMemoryStoreHasNoDurableBacking(t *testing.T) {
	st := NewMemory()
	st.Append(1, Record{Offset: 0, Length: 10, CRC: 1}, true)
	if got := st.MaxSeq(); got != 1 {
		t.Fatalf("MaxSeq = %d, want 1", got)
	}
	if _, ok := st.Get(1); !ok {
		t.Fatal("Get(1) not found in memory-only store")
	}
}
