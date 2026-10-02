package agent

import (
	"context"
	"strconv"
	"strings"
	"time"

	blockhealth "github.com/expanse/expanse/internal/blocks/health"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// probeSyncInterval is how soon a replica placed on this node starts being probed.
const probeSyncInterval = 5 * time.Second

// probeLoop runs the readiness probe of every replica on this node, publishing each result to
// /blocks/<ns>/<name>/status/replicas/<i>, where the load balancer and DNS read it.
func (a *Agent) probeLoop(ctx context.Context) {
	sup := &blockhealth.Supervisor{
		Run: func(ctx context.Context, j blockhealth.Job) {
			r := &blockhealth.Runner{
				Publisher: blockhealth.StorePublisher(a.store, j.BlockKey, j.Index, a.cfg.NodeID),
				Prober:    blockhealth.For(j.Probe.GetType()),
				Target:    j.Target,
				Probe:     j.Probe,
			}
			r.Run(ctx)
		},
		Retire: func(ctx context.Context, j blockhealth.Job) {
			if err := blockhealth.Retire(ctx, a.store, j.BlockKey, j.Index, a.cfg.NodeID); err != nil {
				a.logger.Warn("retire replica probe record", "replica", j.ID(), "err", err)
			}
		},
	}
	a.loop(ctx, probeSyncInterval, "probes", func() { sup.Sync(ctx, a.probeJobs(ctx)) })
}

// probeJobs lists the readiness probes of the replicas placed on this node.
func (a *Agent) probeJobs(ctx context.Context) []blockhealth.Job {
	entries, err := a.store.List(store.WithStale(ctx), store.Key("/node/"+a.cfg.NodeID+"/resources/"))
	if err != nil {
		a.logger.Warn("list replicas to probe", "err", err)
		return nil
	}
	var jobs []blockhealth.Job
	for _, e := range entries {
		ns, name, idx, ok := replicaRef(string(e.Key))
		if !ok {
			continue
		}
		be, err := a.store.Get(store.WithStale(ctx), store.Key("/blocks/"+ns+"/"+name))
		if err != nil {
			continue // block deleted; its replica is on the way out
		}
		var b pb.Block
		if proto.Unmarshal(be.Value, &b) != nil {
			continue
		}
		if j, ok := blockhealth.JobFor(&b, idx, a.lookupNodeIP(a.cfg.NodeID)); ok {
			jobs = append(jobs, j)
		}
	}
	return jobs
}

// replicaRef parses a block-replica resource key: /node/<id>/resources/block-replica:<ns>/<name>/<idx>.
func replicaRef(key string) (ns, name string, idx int32, ok bool) {
	_, ref, found := strings.Cut(key, "/resources/block-replica:")
	parts := strings.Split(ref, "/")
	if !found || len(parts) != 3 {
		return "", "", 0, false
	}
	i, err := strconv.ParseInt(parts[2], 10, 32)
	if err != nil {
		return "", "", 0, false
	}
	return parts[0], parts[1], int32(i), true
}
