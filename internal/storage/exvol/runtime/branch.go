package runtime

import (
	"github.com/expanse/expanse/internal/storage/exvol/oplog"
	pb "github.com/expanse/expanse/proto"
)

// claimsDiverge reports whether a replica's claimed ops are a different
// history than the primary's for the same seqs — e.g. a deposed primary's
// uncommitted writes, which reuse seq numbers the new primary assigned to
// other ops. Op-replay only resends ops past the replica's last seq, so
// such a tail would survive forever; the caller must resync from a snapshot.
func claimsDiverge(claims []*pb.SeqInfo, primary map[uint64]oplog.Record) bool {
	spans := make(map[uint64]oplog.Span, len(primary))
	for seq, rec := range primary {
		spans[seq] = oplog.Span{Offset: uint64(rec.Offset), Length: uint32(rec.Length)}
	}
	overwritten := oplog.NewSpans(spans)
	for _, c := range claims {
		rec, ok := primary[c.GetSeq()]
		if !ok {
			continue // rotated out of the primary's log: nothing to compare
		}
		if (rec.Length == 0) != (c.GetLength() == 0) {
			return true // flush vs data
		}
		if rec.Length == 0 {
			continue
		}
		if uint64(rec.Offset) != c.GetOffset() || uint32(rec.Length) != c.GetLength() {
			return true
		}
		// A later op rewrote these bytes, so a re-recorded CRC is expected.
		if rec.CRC != c.GetCrc32C() && !overwritten.Superseded(c.GetSeq()) {
			return true
		}
	}
	return false
}
