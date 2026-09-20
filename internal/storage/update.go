package storage

import (
	"context"
	"time"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// updateAttempts bounds UpdateStatus's retries against the contended status record.
const updateAttempts = 12

// UpdateStatus applies mutate to a freshly loaded status and writes it back,
// retrying when it loses the race to another writer (sequence reporters, the
// controller). mutate returns false when nothing needs to change. Changing only
// the fields that matter, on the current record, is what keeps a slow caller
// from overwriting somebody else's update with its own stale copy.
func UpdateStatus(ctx context.Context, st store.Store, volID string, mutate func(*Status) bool) error {
	var last error
	for i := 0; i < updateAttempts; i++ {
		s, rev, err := LoadStatus(ctx, st, volID)
		if err != nil {
			return err
		}
		if !mutate(&s) {
			return nil
		}
		if last = CompareAndSwapStatus(ctx, st, volID, rev, s); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return experrors.Wrap(ctx.Err(), experrors.KindTimeout, "storage.UpdateStatus", "canceled")
		case <-time.After(time.Duration(i+1) * 5 * time.Millisecond):
		}
	}
	return last
}
