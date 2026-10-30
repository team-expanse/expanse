package wire

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/store"
)

// Only the leader's bridge writes desired state: a follower's would race it with deletes of its own.
func TestAFollowersBridgeWritesNothing(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	seedPlaced(t, ctx, st, 18080)
	b := &Bridge{St: st, IsLeader: func() bool { return false }}
	if err := b.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if hasKey(ctx, st, "/node/n1/resources/block-replica:default/web/0") {
		t.Error("a follower's bridge wrote desired state")
	}
}

func TestTheBridgeLogsEachDesiredKeyItDeletes(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	stale := "/node/n3/resources/block-replica:default/gone/0"
	if _, err := st.Put(ctx, store.Key(stale), []byte("type: block-replica\n{}")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	b := &Bridge{St: st, Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	if err := b.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if hasKey(ctx, st, stale) || !strings.Contains(buf.String(), stale) {
		t.Errorf("stale key kept or not logged; log %q", buf.String())
	}
}
