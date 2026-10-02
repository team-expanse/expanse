package health

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

// fakeRuns records which jobs are running, started and retired.
type fakeRuns struct {
	mu      sync.Mutex
	running map[string]Job
	starts  int
	retired []string
}

func newFakeRuns() *fakeRuns { return &fakeRuns{running: map[string]Job{}} }

func (f *fakeRuns) supervisor() *Supervisor {
	return &Supervisor{
		Run: func(ctx context.Context, j Job) {
			f.mu.Lock()
			f.running[j.ID()] = j
			f.starts++
			f.mu.Unlock()
			<-ctx.Done()
			f.mu.Lock()
			delete(f.running, j.ID())
			f.mu.Unlock()
		},
		Retire: func(ctx context.Context, j Job) {
			f.mu.Lock()
			f.retired = append(f.retired, j.ID())
			f.mu.Unlock()
		},
	}
}

func (f *fakeRuns) ids() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id := range f.running {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func tcpJob(name string, index int32, port int32) Job {
	return Job{
		BlockKey: store.Key("/blocks/default/" + name), Index: index,
		Probe:  &pb.HealthProbe{Type: pb.ProbeType_PROBE_TCP, Port: port},
		Target: Target{Address: fmt.Sprintf("10.0.0.1:%d", port)},
	}
}

func TestSupervisorStartsAndRetires(t *testing.T) {
	f := newFakeRuns()
	s := f.supervisor()
	ctx := context.Background()
	s.Sync(ctx, []Job{tcpJob("web", 0, 1), tcpJob("db", 1, 2)})
	waitFor(t, "two runners", func() bool { return len(f.ids()) == 2 })

	s.Sync(ctx, []Job{tcpJob("db", 1, 2)})
	waitFor(t, "web's runner to stop", func() bool { return len(f.ids()) == 1 })
	if got := f.ids(); got[0] != "/blocks/default/db/1" {
		t.Errorf("running %v, want only db/1", got)
	}
	if len(f.retired) != 1 || f.retired[0] != "/blocks/default/web/0" {
		t.Errorf("retired %v, want web/0", f.retired)
	}
	if f.starts != 2 {
		t.Errorf("starts = %d; an unchanged job must keep its runner", f.starts)
	}
}

// A changed probe restarts the runner; the replica is still here, so its record stays.
func TestSupervisorRestartsAChangedJob(t *testing.T) {
	f := newFakeRuns()
	s := f.supervisor()
	ctx := context.Background()
	s.Sync(ctx, []Job{tcpJob("web", 0, 1)})
	waitFor(t, "runner", func() bool { return len(f.ids()) == 1 })
	s.Sync(ctx, []Job{tcpJob("web", 0, 2)})
	waitFor(t, "restart", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.starts == 2 && f.running["/blocks/default/web/0"].Probe.GetPort() == 2
	})
	if len(f.retired) != 0 {
		t.Errorf("retired %v on a probe change", f.retired)
	}
}

func TestSupervisorStopAll(t *testing.T) {
	f := newFakeRuns()
	s := f.supervisor()
	s.Sync(context.Background(), []Job{tcpJob("web", 0, 1)})
	waitFor(t, "runner", func() bool { return len(f.ids()) == 1 })
	s.Sync(context.Background(), nil)
	if len(f.ids()) != 0 {
		t.Errorf("still running %v after Sync(nil) returned", f.ids())
	}
}

func block(typ string, probe *pb.HealthProbe) *pb.Block {
	return &pb.Block{
		Metadata: &pb.Metadata{Name: "web", Namespace: "default"},
		Spec: &pb.BlockSpec{Type: typ, Network: &pb.Network{
			HealthCheck: &pb.HealthCheck{Readiness: probe},
		}},
	}
}

func TestJobFor(t *testing.T) {
	tcp := &pb.HealthProbe{Type: pb.ProbeType_PROBE_TCP, Port: 8080, TimeoutSeconds: 3}
	j, ok := JobFor(block("web/whoami", tcp), 2, "10.0.0.5")
	if !ok {
		t.Fatal("no job for a tcp readiness probe")
	}
	if j.ID() != "/blocks/default/web/2" || j.Target.Address != "10.0.0.5:8080" || j.Target.Timeout != 3*time.Second {
		t.Errorf("job = %+v", j)
	}

	http := &pb.HealthProbe{Type: pb.ProbeType_PROBE_HTTP, Port: 80, Path: "/healthz"}
	if j, ok := JobFor(block("web/whoami", http), 0, "fd00::1"); !ok || j.Target.Address != "[fd00::1]:80" || j.Target.Path != "/healthz" {
		t.Errorf("http job = %+v, %t", j, ok)
	}

	for name, b := range map[string]*pb.Block{
		"no probe":   block("web/whoami", nil),
		"none":       block("web/whoami", &pb.HealthProbe{Type: pb.ProbeType_PROBE_NONE}),
		"no port":    block("web/whoami", &pb.HealthProbe{Type: pb.ProbeType_PROBE_TCP}),
		"exec":       block("web/whoami", &pb.HealthProbe{Type: pb.ProbeType_PROBE_EXEC, Command: []string{"true"}}),
		"vm":         block("vm/instance", tcp),
		"no network": {Metadata: &pb.Metadata{Name: "x", Namespace: "default"}, Spec: &pb.BlockSpec{Type: "web/whoami"}},
	} {
		if _, ok := JobFor(b, 0, "10.0.0.5"); ok {
			t.Errorf("%s: got a job, want none", name)
		}
	}
	if _, ok := JobFor(block("web/whoami", tcp), 0, ""); ok {
		t.Error("got a job with no node address")
	}
}
