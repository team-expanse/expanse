package runtime

import (
	"testing"

	"github.com/expanse/expanse/internal/storage/exvol/oplog"
	pb "github.com/expanse/expanse/proto"
)

func seqClaim(seq uint64, off uint64, n uint32, crc uint32) *pb.SeqInfo {
	return &pb.SeqInfo{Seq: seq, Offset: off, Length: n, Crc32C: crc}
}

func TestClaimsDiverge(t *testing.T) {
	primary := map[uint64]oplog.Record{
		1: {Offset: 0, Length: 4096, CRC: 11},
		2: {Offset: 4096, Length: 4096, CRC: 22},
		3: {Offset: 0, Length: 4096, CRC: 33}, // rewrites op 1's range
		4: {Offset: 0, Length: 0, CRC: 0},     // flush
	}
	cases := []struct {
		name   string
		claims []*pb.SeqInfo
		want   bool
	}{
		{"same history", []*pb.SeqInfo{seqClaim(1, 0, 4096, 11), seqClaim(2, 4096, 4096, 22)}, false},
		{"behind but consistent", []*pb.SeqInfo{seqClaim(1, 0, 4096, 11)}, false},
		{"same seq, different data (a deposed primary's own op)", []*pb.SeqInfo{seqClaim(2, 4096, 4096, 99)}, true},
		{"same seq, different range", []*pb.SeqInfo{seqClaim(2, 8192, 4096, 22)}, true},
		{"data op where the primary has a flush", []*pb.SeqInfo{seqClaim(4, 0, 4096, 5)}, true},
		{"flush where the primary has a flush", []*pb.SeqInfo{seqClaim(4, 0, 0, 0)}, false},
		{"overwritten op re-recorded under a fresh CRC is not divergence", []*pb.SeqInfo{seqClaim(1, 0, 4096, 77)}, false},
		{"claim the primary no longer holds", []*pb.SeqInfo{seqClaim(9, 0, 4096, 1)}, false},
	}
	for _, tc := range cases {
		if got := claimsDiverge(tc.claims, primary); got != tc.want {
			t.Errorf("%s: claimsDiverge = %v, want %v", tc.name, got, tc.want)
		}
	}
}
