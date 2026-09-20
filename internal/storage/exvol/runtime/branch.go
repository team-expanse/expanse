package runtime

import (
	"context"

	"github.com/expanse/expanse/internal/storage/exvol/oplog"
	"github.com/expanse/expanse/internal/storage/exvol/protocol"
	"github.com/expanse/expanse/internal/storage/exvol/recovery"
	"github.com/expanse/expanse/internal/storage/exvol/transport"
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

// opReplayVerdict is what a QuerySeq answer says about using op-replay to
// catch a target up.
type opReplayVerdict int

const (
	verdictReplay   opReplayVerdict = iota // a current secondary: resend the ops it lacks
	verdictSnapshot                        // another branch, or ahead of us: only a snapshot resync is honest
	verdictDefer                           // still serving as primary: retry once it has demoted
)

// opReplayVerdictFor classifies a target. A node that still serves as the
// volume's primary (a deposed one that has not noticed yet) answers
// QuerySeq with its last seq and NO claims, so divergence cannot be judged
// and replaying past its seq would leave its uncommitted writes in place.
func opReplayVerdictFor(q *pb.SeqQueryReply, primarySeq uint64, primaryLog map[uint64]oplog.Record) opReplayVerdict {
	switch {
	case q.GetIsPrimary():
		return verdictDefer
	case q.GetLastSeq() > primarySeq:
		return verdictSnapshot
	case claimsDiverge(q.GetOps(), primaryLog):
		return verdictSnapshot
	}
	return verdictReplay
}

// probeFromAnswer turns a QuerySeq answer into a recovery probe. A peer still
// serving as primary reports its last seq without claims: that only bounds what it
// could hold (Answered), so it is not evidence and false is returned.
func probeFromAnswer(nodeID string, q *pb.SeqQueryReply) (recovery.Probe, bool) {
	if q.GetIsPrimary() {
		return recovery.Probe{NodeID: nodeID, Answered: true, LastSeq: q.GetLastSeq()}, false
	}
	crcs := map[uint64]uint32{}
	spans := map[uint64]oplog.Span{}
	for _, op := range q.GetOps() {
		crcs[op.GetSeq()] = op.GetCrc32C()
		spans[op.GetSeq()] = oplog.Span{Offset: op.GetOffset(), Length: op.GetLength()}
	}
	return recovery.Probe{
		NodeID: nodeID, Reachable: true, LastSeq: q.GetLastSeq(),
		CRCs: crcs, Spans: oplog.NewSpans(spans),
	}, true
}

// claimsServeable re-reads a sample of the ops a replica's oplog claims: a hard
// crash can keep the log while losing the zvol bytes, and replaying only the ops
// past its last seq would leave that copy silently wrong.
func claimsServeable(ctx context.Context, conn *transport.Conn, volID, nodeID string, q *pb.SeqQueryReply) bool {
	probe, _ := probeFromAnswer(nodeID, q)
	probe.FetchOps = func(_ context.Context, from, to uint64) ([]protocol.WriteOp, error) {
		rep, err := conn.FetchOps(volID, from, to)
		if err != nil {
			return nil, err
		}
		var ops []protocol.WriteOp
		for _, req := range rep.GetOps() {
			ops = append(ops, protocol.WriteOp{Seq: req.GetSeq(), Offset: req.GetOffset(), Data: req.GetData(), CRC: req.GetCrc32C(), Flush: req.GetFlush()})
		}
		return ops, nil
	}
	return probe.VerifyClaims(ctx) == nil
}

// claimsShareAnOp reports whether any claimed op is also in local with the same
// bytes: the evidence that a peer really is on this node's branch, not merely
// silent about it.
func claimsShareAnOp(claims []*pb.SeqInfo, local map[uint64]oplog.Record) bool {
	for _, c := range claims {
		if rec, ok := local[c.GetSeq()]; ok && rec.Length != 0 && rec.CRC == c.GetCrc32C() && uint64(rec.Offset) == c.GetOffset() {
			return true
		}
	}
	return false
}

// offBranchPeers names the peers whose claims contradict this node's own log when
// this node plus the peers that confirm it hold a quorum: the conflicting ones are
// the minority, typically a deposed primary's tail written while isolated, which
// never reached a quorum and is safe to discard. Without that majority nothing is
// declared off-branch, and a conflict stays a real divergence for a human.
func offBranchPeers(local map[uint64]oplog.Record, peers map[string]*pb.SeqQueryReply, quorum int) map[string]bool {
	off := map[string]bool{}
	agree := 1 // this node
	for id, q := range peers {
		switch {
		case q.GetIsPrimary():
			continue // no claims: nothing to compare
		case claimsDiverge(q.GetOps(), local):
			off[id] = true
		case claimsShareAnOp(q.GetOps(), local):
			agree++
		}
	}
	if agree < quorum {
		return map[string]bool{}
	}
	return off
}
