package agent

import (
	"context"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/blocks/controller"
	"github.com/expanse/expanse/internal/cluster/nodelc"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage/volume"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

func TestAStopAnnouncesARebootOrARestart(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		kind stopKind
		want string // "" = nothing announced
	}{
		{"a stop during shutdown", stopReboot, controller.RebootKey("n1")},
		{"a restart", stopRestart, nodelc.RestartKey("n1")},
		{"a plain stop", stopPlain, ""},
	} {
		st, err := boltstore.New(t.TempDir() + "/bolt.db")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		(&Agent{store: st, cfg: Config{NodeID: "n1"}}).announceStop(c.kind)
		for _, k := range []string{controller.RebootKey("n1"), nodelc.RestartKey("n1")} {
			e, err := st.Get(ctx, store.Key(k))
			switch {
			case k != c.want && !errors.Is(err, errors.KindNotFound):
				t.Errorf("%s: wrote %s: %v", c.name, k, err)
			case k == c.want && err != nil:
				t.Errorf("%s: nothing at %s: %v", c.name, k, err)
			case k == c.want:
				if at, err := time.Parse(time.RFC3339, string(e.Value)); err != nil || time.Since(at) > time.Minute {
					t.Errorf("%s: announcement %q, want the time now", c.name, e.Value)
				}
			}
		}
	}
}

func TestOnlyARestartOfARunningNodeLeavesVolumesAlone(t *testing.T) {
	for _, c := range []struct {
		stopping, restartJob, removed bool
		want                          stopKind
	}{
		{false, true, false, stopRestart},
		{false, false, false, stopPlain}, // systemctl stop: a node-loss stand-in in the VM tests
		{true, true, false, stopReboot},
		{false, true, true, stopPlain},
	} {
		k := stopKindOf(c.stopping, c.restartJob, c.removed)
		if k != c.want {
			t.Errorf("%+v: kind %v, want %v", c, k, c.want)
		}
		if leave := k.volumeStopCause() == volume.ErrRestart; leave != (c.want == stopRestart) {
			t.Errorf("%+v: leaves volumes alone = %v", c, leave)
		}
	}
}

func TestOnlyARestartJobCountsAsARestart(t *testing.T) {
	for out, want := range map[string]bool{
		"1234 expansed.service restart running\n":                                 true,
		"9 sysinit.target start waiting\n1234 expansed.service restart running\n": true,
		"1234 expansed.service stop running\n":                                    false,
		"1234 other.service restart running\n":                                    false,
		"":                                                                        false,
	} {
		if got := restartJobIn(out, "expansed.service"); got != want {
			t.Errorf("%q: %v, want %v", out, got, want)
		}
	}
}
