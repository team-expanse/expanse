package health

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// Job is one replica's readiness probe, run on the replica's node.
type Job struct {
	BlockKey store.Key
	Index    int32
	Probe    *pb.HealthProbe
	Target   Target
}

// ID names the replica the job probes.
func (j Job) ID() string { return fmt.Sprintf("%s/%d", j.BlockKey, j.Index) }

func (j Job) equal(o Job) bool {
	return j.ID() == o.ID() && proto.Equal(j.Probe, o.Probe) && reflect.DeepEqual(j.Target, o.Target)
}

// JobFor builds the readiness job for replica index of b, whose ports listen on host. Exec probes
// are not run yet (they need the unit's namespaces), nor VM probes (the guest reports over vsock).
func JobFor(b *pb.Block, index int32, host string) (Job, bool) {
	p, ok := Readiness(b)
	if !ok || host == "" {
		return Job{}, false
	}
	return Job{
		BlockKey: store.Key("/blocks/" + b.GetMetadata().GetNamespace() + "/" + b.GetMetadata().GetName()),
		Index:    index,
		Probe:    p,
		Target: Target{
			Address: net.JoinHostPort(host, strconv.Itoa(int(p.GetPort()))),
			Path:    p.GetPath(),
			Timeout: time.Duration(p.GetTimeoutSeconds()) * time.Second,
		},
	}, true
}

// Supervisor keeps one running prober per job.
type Supervisor struct {
	// Run probes j until ctx ends.
	Run func(ctx context.Context, j Job)
	// Retire clears what j published, once its replica has left this node.
	Retire func(ctx context.Context, j Job)

	mu      sync.Mutex
	running map[string]*supervised
}

type supervised struct {
	job    Job
	cancel context.CancelFunc
	done   chan struct{}
}

// Sync starts new jobs, restarts changed ones and stops (then retires) the rest. Runners live
// under ctx; stopped runners have exited by the time Sync returns.
func (s *Supervisor) Sync(ctx context.Context, jobs []Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == nil {
		s.running = map[string]*supervised{}
	}
	want := map[string]Job{}
	for _, j := range jobs {
		want[j.ID()] = j
	}
	for id, r := range s.running {
		j, keep := want[id]
		if keep && r.job.equal(j) {
			continue
		}
		r.cancel()
		<-r.done
		delete(s.running, id)
		if !keep && s.Retire != nil {
			s.Retire(ctx, r.job)
		}
	}
	for id, j := range want {
		if _, ok := s.running[id]; !ok {
			s.running[id] = s.start(ctx, j)
		}
	}
}

func (s *Supervisor) start(ctx context.Context, j Job) *supervised {
	rctx, cancel := context.WithCancel(ctx)
	r := &supervised{job: j, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		s.Run(rctx, j)
	}()
	return r
}

// Readiness returns b's readiness probe if agents run it: tcp and http on a port, not on VMs.
func Readiness(b *pb.Block) (*pb.HealthProbe, bool) {
	p := b.GetSpec().GetNetwork().GetHealthCheck().GetReadiness()
	switch {
	case p.GetPort() == 0 || b.GetSpec().GetType() == "vm/instance":
		return nil, false
	case p.GetType() != pb.ProbeType_PROBE_TCP && p.GetType() != pb.ProbeType_PROBE_HTTP:
		return nil, false
	}
	return p, true
}
