package volume

import (
	"context"
	"io"
	"os"

	experrors "github.com/expanse/expanse/internal/errors"
)

const copyChunk = 1 << 20

// copyDevice writes the first n bytes of src over dst and syncs. It opens dst
// exclusively, so the kernel refuses (EBUSY) a device that is mounted or claimed.
func copyDevice(ctx context.Context, src, dst string, n uint64) (err error) {
	const op = "volume.copyDevice"
	in, err := os.Open(src)
	if err != nil {
		return experrors.Wrap(err, experrors.KindInternal, op, "open "+src)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_EXCL, 0)
	if err != nil {
		return experrors.Wrap(err, experrors.KindConflict, op, "open "+dst+" exclusively")
	}
	defer func() {
		if cerr := out.Close(); err == nil && cerr != nil {
			err = experrors.Wrap(cerr, experrors.KindInternal, op, "close "+dst)
		}
	}()
	if _, err := io.CopyBuffer(&limited{ctx: ctx, w: out}, io.LimitReader(in, int64(n)), make([]byte, copyChunk)); err != nil {
		return experrors.Wrap(err, experrors.KindInternal, op, "copy "+src+" to "+dst)
	}
	if err := out.Sync(); err != nil {
		return experrors.Wrap(err, experrors.KindInternal, op, "sync "+dst)
	}
	return nil
}

// limited stops a copy between chunks once its context ends.
type limited struct {
	ctx context.Context
	w   io.Writer
}

func (l *limited) Write(p []byte) (int, error) {
	if err := l.ctx.Err(); err != nil {
		return 0, err
	}
	return l.w.Write(p)
}
