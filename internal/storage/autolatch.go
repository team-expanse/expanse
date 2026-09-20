package storage

import (
	"context"

	"github.com/expanse/expanse/internal/store"
)

// AutoLatchKey marks a NeedsManualRecovery volume whose latch came from the
// controller's no-candidate path: unlike a divergence latch it may lift itself.
// It lives in the store so it outlives the controller's process and leadership.
func AutoLatchKey(volID string) store.Key { return store.Key(VolumePrefix + volID + "/autolatch") }

// SetAutoLatch records that volID's latch is the liftable, no-candidate kind.
func SetAutoLatch(ctx context.Context, st store.Store, volID string) error {
	_, err := st.Put(ctx, AutoLatchKey(volID), []byte("1"))
	return err
}

// AutoLatched reports whether volID's latch is the liftable kind.
func AutoLatched(ctx context.Context, st store.Store, volID string) bool {
	_, err := st.Get(ctx, AutoLatchKey(volID))
	return err == nil
}

// ClearAutoLatch drops the marker (absent is fine).
func ClearAutoLatch(ctx context.Context, st store.Store, volID string) {
	_ = st.Delete(ctx, AutoLatchKey(volID), 0) //nolint:errcheck // an absent marker is the goal
}
