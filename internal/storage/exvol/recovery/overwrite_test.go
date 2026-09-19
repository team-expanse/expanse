package recovery

import (
	"bytes"
	"context"
	"testing"

	"github.com/expanse/expanse/internal/storage/exvol/oplog"
	"github.com/expanse/expanse/internal/storage/exvol/protocol"
)

// overwrittenProbe claims seq 1 with the CRC of its ORIGINAL payload, but
// serves the range's CURRENT bytes (a later op rewrote them).
func overwrittenProbe(spans *oplog.Spans) (Probe, []byte) {
	orig, current := bytes.Repeat([]byte{1}, 4096), bytes.Repeat([]byte{2}, 4096)
	return Probe{
		NodeID: "n2", Reachable: true, LastSeq: 2,
		CRCs:  map[uint64]uint32{1: crc32c(orig), 2: crc32c(current)},
		Spans: spans,
		FetchOps: func(_ context.Context, from, to uint64) ([]protocol.WriteOp, error) {
			var ops []protocol.WriteOp
			for s := from + 1; s <= to; s++ {
				ops = append(ops, protocol.WriteOp{Seq: s, Offset: 0, Data: current, CRC: crc32c(orig)})
			}
			return ops, nil
		},
	}, current
}

func TestFetchHonestServesOverwrittenOp(t *testing.T) {
	spans := oplog.NewSpans(map[uint64]oplog.Span{1: {Offset: 0, Length: 4096}, 2: {Offset: 0, Length: 4096}})
	p, current := overwrittenProbe(spans)
	op, err := fetchHonest(context.Background(), []Probe{p}, map[string]*Probe{"n2": &p}, 1, "n2")
	if err != nil {
		t.Fatalf("an overwritten op is not a dishonest holder: %v", err)
	}
	if !bytes.Equal(op.Data, current) || !op.ValidCRC() {
		t.Fatal("op must carry the current bytes under a CRC that validates them on the wire")
	}
}

func TestFetchHonestStillCatchesTornBytes(t *testing.T) {
	// Nothing overwrote seq 1, so bytes that differ from the claim are corruption.
	p, _ := overwrittenProbe(oplog.NewSpans(map[uint64]oplog.Span{1: {Offset: 0, Length: 4096}, 2: {Offset: 20480, Length: 4096}}))
	if _, err := fetchHonest(context.Background(), []Probe{p}, map[string]*Probe{"n2": &p}, 1, "n2"); err == nil {
		t.Fatal("torn bytes with no overwriting op must still be rejected")
	}
}

func TestDetectDivergenceIgnoresOverwrittenSeq(t *testing.T) {
	spans := oplog.NewSpans(map[uint64]oplog.Span{1: {Offset: 0, Length: 4096}, 2: {Offset: 0, Length: 4096}})
	a := Probe{NodeID: "a", Reachable: true, Spans: spans, CRCs: map[uint64]uint32{1: 0xAAAA, 2: 7}}
	b := Probe{NodeID: "b", Reachable: true, Spans: spans, CRCs: map[uint64]uint32{1: 0xBBBB, 2: 7}}
	if d := detectDivergence([]Probe{a, b}); len(d) != 0 {
		t.Fatalf("a leveled replica re-records overwritten ops under a new CRC; not divergence: %+v", d)
	}
	b.CRCs[2] = 8 // the surviving op really differs
	if d := detectDivergence([]Probe{a, b}); len(d) != 1 || d[0].Seq != 2 {
		t.Fatalf("want divergence at seq 2, got %+v", d)
	}
}
