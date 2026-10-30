package agent

import (
	"context"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/blocks/controller"
	"github.com/expanse/expanse/internal/cluster/nodelc"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

func TestAStopAnnouncesARebootOrARestart(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name     string
		stopping bool
		want     string
		not      string
	}{
		{"a stop during shutdown", true, controller.RebootKey("n1"), nodelc.RestartKey("n1")},
		{"a plain stop", false, nodelc.RestartKey("n1"), controller.RebootKey("n1")},
	} {
		st, err := boltstore.New(t.TempDir() + "/bolt.db")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		(&Agent{store: st, cfg: Config{NodeID: "n1"}}).announceStop(func() bool { return c.stopping })
		e, err := st.Get(ctx, store.Key(c.want))
		if err != nil {
			t.Fatalf("%s: nothing at %s: %v", c.name, c.want, err)
		}
		if at, err := time.Parse(time.RFC3339, string(e.Value)); err != nil || time.Since(at) > time.Minute {
			t.Errorf("%s: announcement %q, want the time now", c.name, e.Value)
		}
		if _, err := st.Get(ctx, store.Key(c.not)); !errors.Is(err, errors.KindNotFound) {
			t.Errorf("%s: also wrote %s: %v", c.name, c.not, err)
		}
	}
}
