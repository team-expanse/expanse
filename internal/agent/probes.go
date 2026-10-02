package agent

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	blockhealth "github.com/expanse/expanse/internal/blocks/health"
	"github.com/expanse/expanse/internal/blocks/runtime/systemd"
	"github.com/expanse/expanse/internal/reconcile"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// probeSyncInterval is how soon a replica placed on this node starts being probed.
const probeSyncInterval = 5 * time.Second

// unitStarter (re)starts a replica's systemd unit.
type unitStarter interface {
	Start(ctx context.Context, unit string) error
}

// probeLoop runs the probes of every replica on this node. Readiness results go to
// /blocks/<ns>/<name>/status/replicas/<i>, where the load balancer and DNS read them; a failing
// liveness probe restarts the replica, recorded at /blocks/<ns>/<name>/status/liveness/<i>.
func (a *Agent) probeLoop(ctx context.Context) {
	sup := &blockhealth.Supervisor{
		Run: func(ctx context.Context, j blockhealth.Job) {
			if j.Liveness {
				a.watchLiveness(ctx, j)
				return
			}
			r := &blockhealth.Runner{
				Publisher: blockhealth.StorePublisher(a.store, j.BlockKey, j.Index, a.cfg.NodeID),
				Prober:    blockhealth.For(j.Probe.GetType()),
				Target:    j.Target,
				Probe:     j.Probe,
			}
			r.Run(ctx)
		},
		Retire: func(ctx context.Context, j blockhealth.Job) {
			retire := blockhealth.Retire
			if j.Liveness {
				retire = blockhealth.RetireLiveness
			}
			if err := retire(ctx, a.store, j.BlockKey, j.Index, a.cfg.NodeID); err != nil {
				a.logger.Warn("retire replica probe record", "replica", j.ID(), "err", err)
			}
		},
	}
	a.loop(ctx, probeSyncInterval, "probes", func() { sup.Sync(ctx, a.probeJobs(ctx)) })
}

// watchLiveness restarts j's unit while its liveness probe fails, until the restarts run out.
func (a *Agent) watchLiveness(ctx context.Context, j blockhealth.Job) {
	publish := blockhealth.LivenessPublisher(a.store, j.BlockKey, j.Index, a.cfg.NodeID)
	w := &blockhealth.Watchdog{
		Prober: blockhealth.For(j.Probe.GetType()),
		Target: j.Target,
		Probe:  j.Probe,
		Restart: func(ctx context.Context) error {
			a.logger.Warn("liveness probe failing; restarting replica", "replica", j.ID(), "unit", j.Unit)
			return a.replicaUnits.Start(ctx, j.Unit)
		},
		Publish: func(ctx context.Context, rec blockhealth.LivenessRecord) error {
			if rec.Failed {
				a.logger.Error("replica still failing its liveness probe; giving up", "replica", j.ID(), "restarts", rec.Restarts, "detail", rec.Detail)
			}
			return publish(ctx, rec)
		},
	}
	a.logger.Info("watching replica liveness", "replica", j.ID(), "unit", j.Unit, "target", j.Target.Address)
	w.Run(ctx)
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
		host := a.lookupNodeIP(a.cfg.NodeID)
		if j, ok := blockhealth.JobFor(&b, idx, host); ok {
			jobs = append(jobs, j)
		}
		var spec systemd.Spec
		_, raw, err := reconcile.SplitType(string(e.Key), e.Value)
		if a.replicaUnits != nil && err == nil && json.Unmarshal(raw, &spec) == nil && spec.Name != "" {
			if j, ok := blockhealth.LivenessJobFor(&b, idx, host, systemd.UnitNameForSpec(spec)); ok {
				jobs = append(jobs, j)
			}
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
