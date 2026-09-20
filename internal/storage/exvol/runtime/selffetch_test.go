package runtime

import (
	"context"
	"testing"

	"github.com/expanse/expanse/internal/storage/exvol/secondary"
)

type memReader []byte

func (m memReader) ReadAt(p []byte, off int64) (int, error) { return copy(p, m[off:]), nil }

// Recovery levels a lagging replica one op at a time; copying the whole op
// log per op made that quadratic (minutes for a 40k-op gap).
func TestSelfFetcherSnapshotsOpLogOnce(t *testing.T) {
	calls := 0
	log := map[uint64]secondary.OpRecord{}
	for seq := uint64(1); seq <= 200; seq++ {
		log[seq] = secondary.OpRecord{Offset: int64(seq%8) * 8, Length: 8, CRC: 1}
	}
	f := newSelfFetcher(func() map[uint64]secondary.OpRecord { calls++; return log }, memReader(make([]byte, 64)))
	for seq := uint64(1); seq <= 200; seq++ {
		if ops, err := f.fetch(context.Background(), seq-1, seq); err != nil || len(ops) != 1 {
			t.Fatalf("fetch %d: ops=%d err=%v", seq, len(ops), err)
		}
	}
	if calls != 1 {
		t.Fatalf("op log copied %d times, want once", calls)
	}
}

func TestSelfFetcherOverwrittenOpCarriesCRCOfCurrentBytes(t *testing.T) {
	buf := make([]byte, 8192)
	copy(buf, "AAAAAAAA")
	copy(buf[4096:], "BBBBBBBB")
	data := memReader(buf)
	log := map[uint64]secondary.OpRecord{
		1: {Offset: 0, Length: 8, CRC: 0xdead}, // rewritten by op 2
		2: {Offset: 0, Length: 8, CRC: 0xbeef},
		3: {Offset: 4096, Length: 8, CRC: crc32c([]byte("BBBBBBBB"))},
	}
	f := newSelfFetcher(func() map[uint64]secondary.OpRecord { return log }, data)
	ops, err := f.fetch(context.Background(), 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := crc32c([]byte("AAAAAAAA")); ops[0].CRC != want {
		t.Errorf("superseded op 1 CRC = %#x, want %#x (current bytes)", ops[0].CRC, want)
	}
	if ops[1].CRC != 0xbeef {
		t.Errorf("latest op 2 CRC = %#x, want its logged 0xbeef", ops[1].CRC)
	}
}

func TestSelfFetcherMissingOpIsUnavailable(t *testing.T) {
	f := newSelfFetcher(func() map[uint64]secondary.OpRecord { return nil }, memReader(nil))
	if _, err := f.fetch(context.Background(), 0, 1); err == nil {
		t.Fatal("an op absent from the local log must fail, not be invented")
	}
}
