package proxy

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/cluster/lease"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// fakeSource is a minimal in-memory Source feeding the watch loop. Like
// boltstore's real Watch, each call registers its own prefix-filtered
// channel — the pool now runs two concurrent watches (blocks, primary
// leases) that must not steal each other's events off one shared chan.
type fakeSource struct {
	mu       sync.Mutex
	entries  map[string][]byte
	rev      store.Revision
	watchers []*fakeWatcher
}

type fakeWatcher struct {
	prefix string
	ch     chan store.Event
}

func newFakeSource() *fakeSource {
	return &fakeSource{entries: map[string][]byte{}}
}

func (f *fakeSource) Get(ctx context.Context, k store.Key) (*store.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.entries[string(k)]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &store.Entry{Key: k, Value: v, Revision: f.rev}, nil
}

func (f *fakeSource) List(ctx context.Context, prefix store.Key) ([]*store.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*store.Entry
	for k, v := range f.entries {
		if len(k) >= len(prefix) && k[:len(prefix)] == string(prefix) {
			out = append(out, &store.Entry{Key: store.Key(k), Value: v, Revision: f.rev})
		}
	}
	return out, nil
}

func (f *fakeSource) Watch(ctx context.Context, prefix store.Key, from store.Revision) (<-chan store.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &fakeWatcher{prefix: string(prefix), ch: make(chan store.Event, 64)}
	f.watchers = append(f.watchers, w)
	return w.ch, nil
}

func (f *fakeSource) Revision(ctx context.Context) (store.Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rev, nil
}

// broadcast delivers ev to every watcher whose prefix matches, mirroring
// boltstore.Store.broadcast. Must be called with f.mu held.
func (f *fakeSource) broadcast(ev store.Event) {
	key := string(ev.Entry.Key)
	for _, w := range f.watchers {
		if strings.HasPrefix(key, w.prefix) {
			w.ch <- ev
		}
	}
}

// put stores a value and emits a Put event (after a small yield so the
// pool's seed pass has run).
func (f *fakeSource) put(t *testing.T, key string, val []byte) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rev++
	rev := f.rev
	f.entries[key] = val
	f.broadcast(store.Event{Type: store.EventPut, Entry: &store.Entry{Key: store.Key(key), Value: val, Revision: rev}})
}

func (f *fakeSource) del(t *testing.T, key string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rev++
	rev := f.rev
	delete(f.entries, key)
	f.broadcast(store.Event{Type: store.EventDelete, Entry: &store.Entry{Key: store.Key(key), Revision: rev}})
}

func blockSpec(t *testing.T, port, target int32) []byte {
	t.Helper()
	b := &pb.Block{
		Spec: &pb.BlockSpec{
			Network: &pb.Network{
				Ports: []*pb.Port{{
					Name: "http", Port: port, TargetPort: target,
					Protocol: "tcp", Expose: pb.Expose_EXPOSE_VIP,
				}},
			},
		},
	}
	out, err := proto.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func statusWith(t *testing.T, placements ...*pb.PlacementStatus) []byte {
	t.Helper()
	out, err := proto.Marshal(&pb.BlockStatus{Placements: placements})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func running(idx int32, node string) *pb.PlacementStatus {
	return &pb.PlacementStatus{ReplicaIndex: idx, NodeId: node, Phase: pb.Phase_RUNNING}
}

func healthJSON(ok bool) []byte {
	out, _ := json.Marshal(healthRecord{OK: ok, Detail: "t"})
	return out
}

func TestPoolBackendLifecycle(t *testing.T) {
	fs := newFakeSource()
	p := NewPool(fs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()

	// Spec first: no service until a status with RUNNING placements lands.
	fs.put(t, "/blocks/default/web", blockSpec(t, 80, 8080))
	fs.put(t, "/blocks/default/web/status", statusWith(t,
		running(0, "n1"), running(1, "n2"),
		&pb.PlacementStatus{ReplicaIndex: 2, NodeId: "n3", Phase: pb.Phase_PENDING},
	))

	deadline := time.Now().Add(5 * time.Second)
	var svc *Service
	for time.Now().Before(deadline) {
		if s := p.Table().Service("default/web"); s != nil && len(s.Backends) == 2 {
			svc = s
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if svc == nil {
		t.Fatalf("service never reached 2 backends: %+v", p.Table().Services)
	}
	if svc.Port != 80 || svc.TargetPort != 8080 || svc.Namespace != "default" || svc.Name != "web" {
		t.Fatalf("service fields wrong: %+v", svc)
	}
	// Only RUNNING placements become backends; sorted by index.
	if len(svc.Backends) != 2 || svc.Backends[0].NodeID != "n1" || svc.Backends[1].NodeID != "n2" {
		t.Fatalf("backends wrong: %+v", svc.Backends)
	}

	// Health record ok=false marks the backend unhealthy; missing
	// records are healthy.
	fs.put(t, "/blocks/default/web/status/replicas/0", healthJSON(false))
	fs.put(t, "/blocks/default/web/status/replicas/1", healthJSON(true))
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := p.Table().Service("default/web"); s != nil && !s.Backends[0].Healthy && s.Backends[1].Healthy {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	svc = p.Table().Service("default/web")
	if svc.Backends[0].Healthy || !svc.Backends[1].Healthy {
		t.Fatalf("health gating wrong: %+v", svc.Backends)
	}
	if got := svc.Healthy(); len(got) != 1 || got[0].NodeID != "n2" {
		t.Fatalf("Healthy() wrong: %+v", got)
	}

	// LOST placement removes the backend; deleting the block spec or its
	// status removes the service.
	fs.put(t, "/blocks/default/web/status", statusWith(t, running(0, "n1")))
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := p.Table().Service("default/web"); s != nil && len(s.Backends) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	fs.del(t, "/blocks/default/web/status")
	fs.del(t, "/blocks/default/web")
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p.Table().Service("default/web") == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("service never disappeared after delete: %+v", p.Table().Services)
}

func TestPoolConcurrentReadersDuringRebuild(t *testing.T) {
	fs := newFakeSource()
	p := NewPool(fs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()

	fs.put(t, "/blocks/default/web", blockSpec(t, 80, 8080))
	fs.put(t, "/blocks/default/web/status", statusWith(t, running(0, "n1"), running(1, "n2"), running(2, "n3")))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	// Hammer Table() while rebuilds churn — the race detector proves
	// readers never see a torn or half-updated snapshot.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				tab := p.Table()
				for _, s := range tab.Services {
					if s.Port != 80 {
						t.Errorf("torn read: port %d", s.Port)
					}
					for _, b := range s.Backends {
						if b.NodeID != "n1" && b.NodeID != "n2" && b.NodeID != "n3" && b.NodeID != "n4" {
							t.Errorf("torn read: node %q", b.NodeID)
						}
					}
				}
			}
		}()
	}
	for round := 0; round < 200; round++ {
		fs.put(t, "/blocks/default/web/status", statusWith(t,
			running(0, "n1"), running(1, "n2"), running(int32(round%3+1), "n4")))
		fs.del(t, "/blocks/default/web/status/replicas/0")
		fs.put(t, "/blocks/default/web/status/replicas/0", healthJSON(true))
	}
	close(stop)
	wg.Wait()
}

// pgBlockSpec is blockSpec plus spec.type, the signal LB wiring (D2)
// uses to pick a primary-only routing mode.
func pgBlockSpec(t *testing.T, typ string, port, target int32) []byte {
	t.Helper()
	b := &pb.Block{
		Spec: &pb.BlockSpec{
			Type: typ,
			Network: &pb.Network{
				Ports: []*pb.Port{{
					Name: "pg", Port: port, TargetPort: target,
					Protocol: "tcp", Expose: pb.Expose_EXPOSE_VIP,
				}},
			},
		},
	}
	out, err := proto.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// leaseJSON encodes a pg-primary lease record exactly as
// internal/cluster/lease's Manager would store it.
func leaseJSON(t *testing.T, holder string) []byte {
	t.Helper()
	out, err := lease.EncodeForTest(lease.Lease{
		Holder: holder, ExpiresAt: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPoolTracksPrimaryLease(t *testing.T) {
	fs := newFakeSource()
	p := NewPool(fs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()

	fs.put(t, "/blocks/default/pg", pgBlockSpec(t, "db/postgres", 5432, 5432))
	fs.put(t, "/blocks/default/pg/status", statusWith(t, running(0, "n1"), running(1, "n2")))

	deadline := time.Now().Add(5 * time.Second)
	var svc *Service
	for time.Now().Before(deadline) {
		if s := p.Table().Service("default/pg"); s != nil && len(s.Backends) == 2 {
			svc = s
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if svc == nil {
		t.Fatalf("service never reached 2 backends: %+v", p.Table().Services)
	}
	if svc.Type != "db/postgres" {
		t.Fatalf("Type = %q, want db/postgres", svc.Type)
	}
	if _, ok := svc.Primary(); ok {
		t.Fatal("Primary() true before any election lease exists")
	}

	// The election lease names n2 primary: Service.Primary() must follow
	// it without waiting on any /blocks/ event.
	fs.put(t, "/leases/pg-primary:default/pg", leaseJSON(t, "n2"))
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, ok := p.Table().Service("default/pg").Primary(); ok && b.NodeID == "n2" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	b, ok := p.Table().Service("default/pg").Primary()
	if !ok || b.NodeID != "n2" {
		t.Fatalf("Primary() = %+v, %v; want n2, true", b, ok)
	}

	// Lease released (owner shut down, or instance disappeared): Primary
	// must go back to false, not stick to the last known holder.
	fs.del(t, "/leases/pg-primary:default/pg")
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := p.Table().Service("default/pg").Primary(); !ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("Primary() still true after the lease was deleted")
}

func TestPoolNoVIPPortNoService(t *testing.T) {
	fs := newFakeSource()
	p := NewPool(fs)
	b := &pb.Block{Spec: &pb.BlockSpec{Network: &pb.Network{
		Ports: []*pb.Port{{Name: "x", Port: 90, Protocol: "tcp"}},
	}}}
	raw, _ := proto.Marshal(b)
	fs.put(t, "/blocks/default/plain", raw)
	fs.put(t, "/blocks/default/plain/status", statusWith(t, running(0, "n1")))
	time.Sleep(50 * time.Millisecond)
	if s := p.Table().Service("default/plain"); s != nil {
		t.Fatalf("non-VIP port produced a service: %+v", s)
	}
}
