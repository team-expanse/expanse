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

func TestOpReplayVerdict(t *testing.T) {
	log := map[uint64]oplog.Record{9: {Offset: 16384, Length: 4096, CRC: 7}}
	same := &pb.SeqInfo{Seq: 9, Offset: 16384, Length: 4096, Crc32C: 7}
	other := &pb.SeqInfo{Seq: 9, Offset: 81920, Length: 4096, Crc32C: 1}
	cases := []struct {
		name string
		q    *pb.SeqQueryReply
		want opReplayVerdict
	}{
		{"current secondary", &pb.SeqQueryReply{LastSeq: 9, Ops: []*pb.SeqInfo{same}}, verdictReplay},
		{"still serving as primary: its answer carries no claims", &pb.SeqQueryReply{LastSeq: 9, IsPrimary: true}, verdictDefer},
		{"ahead of the primary", &pb.SeqQueryReply{LastSeq: 30}, verdictSnapshot},
		{"another branch", &pb.SeqQueryReply{LastSeq: 9, Ops: []*pb.SeqInfo{other}}, verdictSnapshot},
	}
	for _, c := range cases {
		if got := opReplayVerdictFor(c.q, 24, log); got != c.want {
			t.Errorf("%s: verdict %v, want %v", c.name, got, c.want)
		}
	}
}

// A peer still serving as primary answers with a last seq and no claims: that is a
// bound on what it may hold, not evidence, so recovery must not source ops from it.
func TestPrimaryAnswerIsABoundNotEvidence(t *testing.T) {
	q := &pb.SeqQueryReply{LastSeq: 90, IsPrimary: true}
	p, evidence := probeFromAnswer("n1", q)
	if evidence || p.Reachable {
		t.Fatalf("a still-serving primary must not count as a reachable replica: %+v", p)
	}
	if !p.Answered || p.LastSeq != 90 {
		t.Fatalf("its last seq must still bound what it could hold: %+v", p)
	}
	if _, evidence := probeFromAnswer("n2", &pb.SeqQueryReply{LastSeq: 90}); !evidence {
		t.Fatal("an ordinary secondary's answer is evidence")
	}
}

func answer(claims ...*pb.SeqInfo) *pb.SeqQueryReply {
	return &pb.SeqQueryReply{LastSeq: 9, Ops: claims}
}

// A deposed primary's tail (written while isolated, never acked) conflicts with
// the branch this node holds; with a quorum on this node's side it is the minority.
func TestOffBranchPeersAreTheMinorityAgainstAQuorum(t *testing.T) {
	local := map[uint64]oplog.Record{5: {Offset: 0, Length: 4096, CRC: 55}, 6: {Offset: 4096, Length: 4096, CRC: 66}}
	peers := map[string]*pb.SeqQueryReply{
		"n1": answer(seqClaim(5, 0, 4096, 55), seqClaim(6, 4096, 4096, 999)), // conflicts at 6
		"n2": answer(seqClaim(5, 0, 4096, 55), seqClaim(6, 4096, 4096, 66)),  // agrees
	}
	off := offBranchPeers(local, peers, 2) // R=3: this node + n2 is a quorum
	if !off["n1"] || off["n2"] || len(off) != 1 {
		t.Fatalf("off-branch = %v, want only n1", off)
	}
}

func TestNoPeerIsOffBranchWithoutAQuorumOnThisSide(t *testing.T) {
	local := map[uint64]oplog.Record{6: {Offset: 4096, Length: 4096, CRC: 66}}
	peers := map[string]*pb.SeqQueryReply{"n1": answer(seqClaim(6, 4096, 4096, 999))} // 1 vs 1: a real split
	if off := offBranchPeers(local, peers, 2); len(off) != 0 {
		t.Fatalf("off-branch = %v: without a majority the conflict is a genuine divergence", off)
	}
}

func TestPeersThatOnlyShareNothingDoNotCountAsAgreeing(t *testing.T) {
	local := map[uint64]oplog.Record{6: {Offset: 4096, Length: 4096, CRC: 66}}
	peers := map[string]*pb.SeqQueryReply{
		"n1": answer(seqClaim(6, 4096, 4096, 999)), // conflicts
		"n2": answer(seqClaim(20, 0, 4096, 7)),     // overlaps nothing: proves nothing
	}
	if off := offBranchPeers(local, peers, 2); len(off) != 0 {
		t.Fatalf("off-branch = %v: n2 does not confirm this node's branch", off)
	}
}
