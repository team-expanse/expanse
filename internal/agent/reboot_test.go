package agent

import (
	"context"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/blocks/controller"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

func TestAStopDuringShutdownAnnouncesTheReboot(t *testing.T) {
	st, err := boltstore.New(t.TempDir() + "/bolt.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	key := store.Key(controller.RebootKey("n1"))

	(&Agent{store: st, cfg: Config{NodeID: "n1"}}).announceReboot(func() bool { return false })
	if _, err := st.Get(ctx, key); !errors.Is(err, errors.KindNotFound) {
		t.Fatalf("a plain stop announced a reboot: %v", err)
	}
	(&Agent{store: st, cfg: Config{NodeID: "n1"}}).announceReboot(func() bool { return true })
	e, err := st.Get(ctx, key)
	if err != nil {
		t.Fatalf("no reboot announced: %v", err)
	}
	if at, err := time.Parse(time.RFC3339, string(e.Value)); err != nil || time.Since(at) > time.Minute {
		t.Errorf("announcement %q, want the time now", e.Value)
	}

	a := &Agent{store: st, cfg: Config{NodeID: "n1"}}
	a.clearReboot(ctx)
	if _, err := st.Get(ctx, key); !errors.Is(err, errors.KindNotFound) {
		t.Errorf("the node back up kept its reboot announcement: %v", err)
	}
	(&Agent{store: st, cfg: Config{NodeID: "n1"}}).announceReboot(func() bool { return true })
	a.clearReboot(ctx) // once per run: a later status write must not cost a store write each time
	if _, err := st.Get(ctx, key); err != nil {
		t.Errorf("a second clear in the same run touched the store: %v", err)
	}
}
