package runtime

import (
	"context"
	"fmt"
	"io"
	"sync"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage/exvol/oplog"
	"github.com/expanse/expanse/internal/storage/exvol/protocol"
	"github.com/expanse/expanse/internal/storage/exvol/secondary"
)

// selfFetcher re-reads ops from THIS node's durable copy for recovery's
// leveling (4c): claims from the local op log, bytes from the zvol. The
// log is copied once, on first use (recovery does not write), because
// leveling asks for one op at a time and a copy per op is quadratic.
type selfFetcher struct {
	readLog func() map[uint64]secondary.OpRecord
	data    io.ReaderAt

	once  sync.Once
	log   map[uint64]secondary.OpRecord
	spans *oplog.Spans
}

func newSelfFetcher(readLog func() map[uint64]secondary.OpRecord, data io.ReaderAt) *selfFetcher {
	return &selfFetcher{readLog: readLog, data: data}
}

func (f *selfFetcher) load() {
	f.log = f.readLog()
	m := make(map[uint64]oplog.Span, len(f.log))
	for seq, rec := range f.log {
		m[seq] = oplog.Span{Offset: uint64(rec.Offset), Length: uint32(rec.Length)}
	}
	f.spans = oplog.NewSpans(m)
}

// fetch returns ops (from, to]. An op a later op rewrote is described by
// the CRC of the bytes now on disk, since its logged CRC no longer applies.
func (f *selfFetcher) fetch(_ context.Context, from, to uint64) ([]protocol.WriteOp, error) {
	f.once.Do(f.load)
	ops := make([]protocol.WriteOp, 0, to-from)
	for seq := from + 1; seq <= to; seq++ {
		rec, ok := f.log[seq]
		if !ok {
			return nil, experrors.New(experrors.KindUnavailable, "exvol.recoverVol.selfFetch",
				fmt.Sprintf("op %d not in local oplog (filled by pull, not logged) — target must resync", seq))
		}
		op := protocol.WriteOp{Seq: seq, Offset: uint64(rec.Offset), CRC: rec.CRC}
		if rec.Length == 0 {
			op.Flush = true
			ops = append(ops, op)
			continue
		}
		buf := make([]byte, rec.Length)
		if _, err := f.data.ReadAt(buf, rec.Offset); err != nil {
			return nil, experrors.Wrap(err, experrors.KindInternal, "exvol.recoverVol.selfFetch", "self re-read")
		}
		op.Data = buf
		if f.spans.Superseded(seq) {
			op.CRC = crc32c(buf)
		}
		ops = append(ops, op)
	}
	return ops, nil
}
