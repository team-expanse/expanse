package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/agent/nix"
	"github.com/expanse/expanse/internal/osgen"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

type genRig struct {
	g       *nodeGenerations
	st      *boltstore.Store
	gens    []nix.Generation
	cleaned [][]int
}

func newGenRig(t *testing.T) *genRig {
	t.Helper()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	r := &genRig{st: st}
	for n := 1; n <= 4; n++ {
		r.gens = append(r.gens, nix.Generation{Number: n, Path: nix.StorePath("/nix/store/sys" + string(rune('0'+n))), Date: day.AddDate(0, 0, n)})
	}
	releases := filepath.Join(t.TempDir(), "release-systems.json")
	osgen.RecordReleaseSystem(releases, "/nix/store/sys3", "1.14.0")
	r.g = &nodeGenerations{
		node: "n1", releaseSystems: releases, log: slog.Default(),
		list:    func(context.Context) ([]nix.Generation, error) { return r.gens, nil },
		current: func() string { return "/nix/store/sys4" },
		booted:  func() string { return "/nix/store/sys3" },
		releaseOf: func(sys string) string {
			if sys == "/nix/store/sys1" {
				return "1.13.1"
			}
			return "1.14.0"
		},
		clean: func(_ context.Context, ns []int) error { r.cleaned = append(r.cleaned, ns); return nil },
	}
	return r
}

func (r *genRig) report(t *testing.T) (osgen.Report, store.Revision) {
	t.Helper()
	e, err := r.st.Get(context.Background(), osgen.ReportKey("n1"))
	if err != nil {
		t.Fatal(err)
	}
	var rep osgen.Report
	if err := json.Unmarshal(e.Value, &rep); err != nil {
		t.Fatal(err)
	}
	return rep, e.Revision
}

func TestGenerationsPassPublishesReleasesAndDrift(t *testing.T) {
	r := newGenRig(t)
	r.g.pass(context.Background(), r.st)
	rep, _ := r.report(t)
	want := []osgen.Generation{
		{Number: 4, Date: r.gens[3].Date, Release: "1.14.0", Recorded: true, Current: true},
		{Number: 3, Date: r.gens[2].Date, Release: "1.14.0", Exact: true, Recorded: true, Booted: true},
		{Number: 2, Date: r.gens[1].Date, Release: "1.14.0", Recorded: true},
		{Number: 1, Date: r.gens[0].Date, Release: "1.13.1"},
	}
	if !reflect.DeepEqual(rep.Generations, want) {
		t.Errorf("generations =\n%+v\nwant\n%+v", rep.Generations, want)
	}
	if got := osgen.Drift(rep.Generations); got != "1.14.0, modified" {
		t.Errorf("drift = %q", got)
	}
}

// An idle node must not rewrite its report every pass: Activity would show each one.
func TestGenerationsPassWritesOnlyChanges(t *testing.T) {
	r := newGenRig(t)
	r.g.pass(context.Background(), r.st)
	_, rev := r.report(t)
	r.g.pass(context.Background(), r.st)
	if _, again := r.report(t); again != rev {
		t.Errorf("an unchanged report was rewritten (revision %d -> %d)", rev, again)
	}
}

func TestGenerationsPassCleansOnRequest(t *testing.T) {
	r := newGenRig(t)
	ctx := context.Background()
	b, _ := json.Marshal(osgen.CleanRequest{Keep: 1})
	if _, err := r.st.Put(ctx, osgen.RequestKey("n1"), b); err != nil {
		t.Fatal(err)
	}
	r.g.pass(ctx, r.st)
	if want := [][]int{{2, 1}}; !reflect.DeepEqual(r.cleaned, want) {
		t.Errorf("cleaned %v, want %v: the running (4) and booted (3) stay", r.cleaned, want)
	}
	if _, err := r.st.Get(ctx, osgen.RequestKey("n1")); err == nil {
		t.Error("the request is still there after the cleanup")
	}
	rep, _ := r.report(t)
	if rep.Clean == nil || rep.Clean.Keep != 1 || !reflect.DeepEqual(rep.Clean.Removed, []int{2, 1}) || rep.Clean.Error != "" {
		t.Errorf("clean result = %+v", rep.Clean)
	}
}
