package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"slices"
	"time"

	"github.com/expanse/expanse/internal/agent/nix"
	"github.com/expanse/expanse/internal/osgen"
	"github.com/expanse/expanse/internal/store"
)

const generationsPassInterval = 15 * time.Second

// nodeGenerations publishes this node's system generations and runs the web UI's cleanup requests.
type nodeGenerations struct {
	node            string
	releaseSystems  string
	list            func(context.Context) ([]nix.Generation, error)
	current, booted func() string
	releaseOf       func(system string) string
	clean           func(context.Context, []int) error
	log             *slog.Logger

	published []byte
	lastClean *osgen.CleanResult
}

func (a *Agent) generationsLoop(ctx context.Context) {
	d := nix.New()
	resolve := func(link string) func() string {
		return func() string { p, _ := filepath.EvalSymlinks(link); return p }
	}
	g := &nodeGenerations{
		node: a.cfg.NodeID, releaseSystems: osgen.DefaultReleaseSystemsPath, log: a.logger,
		list: d.Generations, current: resolve("/run/current-system"), booted: resolve("/run/booted-system"),
		releaseOf: osgen.ReleaseOf, clean: d.DeleteGenerations,
	}
	a.loop(ctx, generationsPassInterval, "generations", func() { g.pass(ctx, a.store) })
}

func (g *nodeGenerations) pass(ctx context.Context, st store.Store) {
	gens, err := g.generations(ctx)
	if err != nil {
		g.log.Warn("system generations unavailable", "err", err)
		return
	}
	if e, err := st.Get(ctx, osgen.RequestKey(g.node)); err == nil {
		gens = g.runClean(ctx, gens, e)
		if err := st.Delete(ctx, e.Key, e.Revision); err != nil {
			g.log.Warn("clear the generations cleanup request", "err", err)
		}
	}
	b, err := json.Marshal(osgen.Report{Generations: gens, Clean: g.lastClean})
	if err != nil || bytes.Equal(b, g.published) {
		return
	}
	if _, err := st.Put(ctx, osgen.ReportKey(g.node), b); err != nil {
		g.log.Warn("publish system generations", "err", err)
		return
	}
	g.published = b
}

// runClean deletes what req's keep leaves over, and returns the generations as they are after.
func (g *nodeGenerations) runClean(ctx context.Context, gens []osgen.Generation, e *store.Entry) []osgen.Generation {
	var req osgen.CleanRequest
	res := &osgen.CleanResult{At: time.Now().UTC()}
	if err := json.Unmarshal(e.Value, &req); err != nil {
		res.Error = "unreadable request: " + err.Error()
	} else {
		res.Keep, res.Removed = req.Keep, osgen.ToRemove(gens, req.Keep)
		if err := g.clean(ctx, res.Removed); err != nil {
			res.Error = err.Error()
		}
		g.log.Info("cleaned up system generations", "keep", req.Keep, "removed", res.Removed, "err", res.Error)
	}
	g.lastClean = res
	if after, err := g.generations(ctx); err == nil {
		return after
	}
	return gens
}

// generations lists the node's generations newest first, each with its release.
func (g *nodeGenerations) generations(ctx context.Context) ([]osgen.Generation, error) {
	gens, err := g.list(ctx)
	if err != nil {
		return nil, err
	}
	records := osgen.LoadReleaseSystems(g.releaseSystems)
	recorded := map[string]bool{}
	for _, v := range records {
		recorded[v] = true
	}
	current, booted := g.current(), g.booted()
	out := make([]osgen.Generation, 0, len(gens))
	for _, n := range slices.Backward(gens) {
		path := string(n.Path)
		rel := g.releaseOf(path)
		out = append(out, osgen.Generation{
			Number: n.Number, Date: n.Date.UTC(), Release: rel,
			Exact: rel != "" && records[path] == rel, Recorded: recorded[rel],
			Current: path == current, Booted: path == booted,
		})
	}
	return out, nil
}
