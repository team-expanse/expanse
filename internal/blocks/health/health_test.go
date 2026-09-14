package health

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/blocks/lifecycle"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
)

// ---- Prober fixtures ----

func TestTCPProber(t *testing.T) {
	// Pass: raw listener accepting.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	ctx := context.Background()
	if r := (TCPProber{}).Probe(ctx, Target{Address: ln.Addr().String(), Timeout: time.Second}); !r.OK {
		t.Errorf("tcp pass failed: %+v", r)
	}
	// Fail: nothing listening.
	if r := (TCPProber{}).Probe(ctx, Target{Address: "127.0.0.1:1", Timeout: 200 * time.Millisecond}); r.OK {
		t.Error("tcp to closed port passed")
	}
}

func TestHTTPProber(t *testing.T) {
	ctx := context.Background()
	// Pass: 200.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(200)
		case "/teapot":
			w.WriteHeader(418)
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound) // 302 is a pass (≤399)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound) // must NOT be followed
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	if r := (HTTPProber{}).Probe(ctx, Target{Address: addr, Path: "/ok"}); !r.OK {
		t.Errorf("200 failed: %+v", r)
	}
	if r := (HTTPProber{}).Probe(ctx, Target{Address: addr, Path: "/redirect"}); !r.OK {
		t.Errorf("302 (200-399) should pass: %+v", r)
	}
	if r := (HTTPProber{}).Probe(ctx, Target{Address: addr, Path: "/teapot"}); r.OK {
		t.Error("418 should fail (outside 200-399)")
	}
	// No redirect following: /loop must terminate as its own (passing)
	// response, not loop/fail on redirects — and not hang.
	done := make(chan Result, 1)
	go func() { done <- (HTTPProber{}).Probe(ctx, Target{Address: addr, Path: "/loop", Timeout: time.Second}) }()
	select {
	case r := <-done:
		if !r.OK || r.Detail != "HTTP 302" {
			t.Errorf("redirect not followed, want HTTP 302 result: %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("probe followed a redirect loop")
	}
	// 5xx fails.
	if r := (HTTPProber{}).Probe(ctx, Target{Address: addr, Path: "/bad"}); r.OK {
		t.Error("500 passed")
	}
}

func TestExecProber(t *testing.T) {
	ctx := context.Background()
	if r := (ExecProber{}).Probe(ctx, Target{Command: []string{"true"}}); !r.OK {
		t.Errorf("true failed: %+v", r)
	}
	r := (ExecProber{}).Probe(ctx, Target{Command: []string{"false"}})
	if r.OK {
		t.Error("false passed")
	}
	if r.Err == "" {
		t.Error("exec failure must carry error detail")
	}
	// Timeout bounded.
	r = (ExecProber{}).Probe(ctx, Target{Command: []string{"sleep", "5"}, Timeout: 300 * time.Millisecond})
	if r.OK {
		t.Error("timed-out exec passed")
	}
	// Output surfaces in Detail.
	r = (ExecProber{}).Probe(ctx, Target{Command: []string{"echo", "hello"}})
	if !r.OK || r.Detail != "hello\n" {
		t.Errorf("exec detail = %q", r.Detail)
	}
}

func TestNoneProberAndFor(t *testing.T) {
	ctx := context.Background()
	if r := (NoneProber{}).Probe(ctx, Target{}); !r.OK {
		t.Error("none should always pass")
	}
	if _, ok := For(pb.ProbeType_PROBE_NONE).(NoneProber); !ok {
		t.Error("unspecified/none maps to NoneProber")
	}
	if _, ok := For(pb.ProbeType_PROBE_TCP).(TCPProber); !ok {
		t.Error("tcp maps to TCPProber")
	}
}

// ---- Runner: write-on-transition + heartbeats ----

type memPublisher struct {
	mu      sync.Mutex
	writes  []Record
	blocker chan struct{} // when non-nil, blocks writes (simulates slow store)
}

func (m *memPublisher) publish(ctx context.Context, rec Record) error {
	if m.blocker != nil {
		select {
		case <-m.blocker:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.mu.Lock()
	m.writes = append(m.writes, rec)
	m.mu.Unlock()
	return nil
}

func (m *memPublisher) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.writes)
}

func (m *memPublisher) last() Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writes[len(m.writes)-1]
}

type fakeProbe struct {
	mu   sync.Mutex
	pass bool
}

func (f *fakeProbe) Probe(ctx context.Context, t Target) Result {
	f.mu.Lock()
	pass := f.pass
	f.mu.Unlock()
	return Result{OK: pass, Detail: fmt.Sprintf("pass=%v", pass), At: time.Now()}
}

// N consecutive identical results produce exactly 1 transition write, not
// N writes; heartbeats fill the gaps (§5.4).
func TestWriteOnTransitionOnly(t *testing.T) {
	p := &fakeProbe{pass: true}
	pub := &memPublisher{}
	r := &Runner{
		Publisher: pub.publish,
		Prober:    p,
		Target:    Target{Address: "127.0.0.1:1"},
		Probe:     &pb.HealthProbe{SuccessThreshold: 1, FailureThreshold: 1},
		Period:    10 * time.Millisecond,
		Heartbeat: 40 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	// ~10 identical passing results over 100ms: 1 heartbeat (t≈40,80) + 0
	// transitions (first result only seeds state).
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	n := pub.count()
	if n < 1 || n > 4 {
		t.Errorf("writes = %d for ~10 identical passing probes; want only heartbeats (1-4)", n)
	}
	for i, rec := range pub.writes {
		if !rec.Heartbeat {
			t.Errorf("write %d is a transition but no result changed: %+v", i, rec)
		}
	}
}

// A transition produces exactly one non-heartbeat write plus heartbeats.
func TestTransitionWrite(t *testing.T) {
	p := &fakeProbe{pass: true}
	pub := &memPublisher{}
	r := &Runner{
		Publisher: pub.publish,
		Prober:    p,
		Probe:     &pb.HealthProbe{SuccessThreshold: 1, FailureThreshold: 1},
		Period:    10 * time.Millisecond,
		Heartbeat: time.Hour, // no heartbeat noise in this window
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	time.Sleep(30 * time.Millisecond) // seeds healthy silently
	p.mu.Lock()
	p.pass = false // flip: exactly 1 transition write
	p.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if got := pub.count(); got != 1 {
		t.Errorf("writes = %d after one flip, want exactly 1", got)
	}
	rec := pub.last()
	if rec.OK || rec.Heartbeat {
		t.Errorf("transition record wrong: %+v", rec)
	}
}

// Threshold counting drives the expected event — driven synchronously via
// ProbeOnce so no wall-clock races: failureThreshold=2 absorbs one fail
// and fires exactly one EventReadinessFail on the second consecutive fail.
func TestThresholdCountingDrivesEvents(t *testing.T) {
	var mu sync.Mutex
	var events []lifecycle.Event
	r := &Runner{
		Prober: &fakeProbe{pass: true},
		Probe:  &pb.HealthProbe{SuccessThreshold: 1, FailureThreshold: 2},
		OnEvent: func(ev lifecycle.Event) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		},
	}
	ctx := context.Background()
	r.ProbeOnce(ctx) // seed healthy, silent
	r.ProbeOnce(ctx) // still pass: no event
	mu.Lock()
	if len(events) != 0 {
		t.Fatalf("passing probes fired events: %v", events)
	}
	mu.Unlock()

	r.Prober = &fakeProbe{pass: false}
	r.ProbeOnce(ctx) // fail 1: absorbed
	mu.Lock()
	if len(events) != 0 {
		t.Errorf("event fired after 1 fail with failureThreshold=2: %v", events)
	}
	mu.Unlock()

	r.ProbeOnce(ctx) // fail 2: fires
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0] != lifecycle.EventReadinessFail {
		t.Errorf("events = %v, want exactly one EventReadinessFail", events)
	}
}

// successThreshold: consecutive passes required to re-fire readiness pass.
func TestSuccessThreshold(t *testing.T) {
	var events []lifecycle.Event
	r := &Runner{
		Prober: &fakeProbe{pass: false},
		Probe:  &pb.HealthProbe{SuccessThreshold: 2, FailureThreshold: 1},
		OnEvent: func(ev lifecycle.Event) {
			events = append(events, ev)
		},
	}
	ctx := context.Background()
	r.ProbeOnce(ctx) // seeds unhealthy silently
	r.Prober = &fakeProbe{pass: true}
	r.ProbeOnce(ctx) // pass 1: absorbed
	if len(events) != 0 {
		t.Fatalf("event after 1 pass with successThreshold=2: %v", events)
	}
	r.ProbeOnce(ctx) // pass 2: fires
	if len(events) != 1 || events[0] != lifecycle.EventReadinessPass {
		t.Errorf("events = %v, want exactly one EventReadinessPass", events)
	}
}

// Liveness probers emit EventLivenessFail on failure.
func TestLivenessEvents(t *testing.T) {
	var mu sync.Mutex
	var events []lifecycle.Event
	p := &fakeProbe{pass: true}
	r := &Runner{
		Prober:   p,
		Probe:    &pb.HealthProbe{},
		Period:   10 * time.Millisecond,
		Liveness: true,
		OnEvent: func(ev lifecycle.Event) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	time.Sleep(30 * time.Millisecond)
	p.mu.Lock()
	p.pass = false
	p.mu.Unlock()
	time.Sleep(40 * time.Millisecond)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0] != lifecycle.EventLivenessFail {
		t.Errorf("events = %v, want [EventLivenessFail]", events)
	}
}

// Heartbeat fires when nothing changes for the heartbeat period.
func TestHeartbeatPeriodic(t *testing.T) {
	p := &fakeProbe{pass: true}
	pub := &memPublisher{}
	r := &Runner{
		Publisher: pub.publish,
		Prober:    p,
		Probe:     &pb.HealthProbe{},
		Period:    10 * time.Millisecond,
		Heartbeat: 30 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done
	// ~100ms with 30ms heartbeats → at least 2, all heartbeats.
	if n := pub.count(); n < 2 {
		t.Errorf("heartbeats = %d, want >= 2 over 100ms", n)
	}
	for _, rec := range pub.writes {
		if !rec.Heartbeat {
			t.Errorf("non-heartbeat write with no transition: %+v", rec)
		}
	}
}

// ---- Store publisher (linearizable, CAS) ----

func newStore(t *testing.T) *raftstore.Store {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.Close()
	st, err := raftstore.Open(raftstore.Config{
		NodeID:    "n1",
		BindAddr:  ln.Addr().String(),
		DataDir:   t.TempDir(),
		Bootstrap: true,
		LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && st.Leader() == "" {
		time.Sleep(20 * time.Millisecond)
	}
	return st
}

func TestStorePublisherRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	// Seed the block so the status subtree has a parent (not required by
	// the store, but keeps the layout honest).
	blk, _ := json.Marshal(map[string]string{"name": "web"})
	if _, err := st.Txn(ctx, []store.Op{{
		Kind: store.OpPut,
		Key:  "/blocks/default/web", Value: blk,
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pub := StorePublisher(st, "/blocks/default/web", 2)
	rec := Record{OK: false, Detail: "HTTP 503", At: time.Now(), LatencyNs: 1234}
	if err := pub(ctx, rec); err != nil {
		t.Fatalf("publish: %v", err)
	}
	e, err := st.Get(ctx, "/blocks/default/web/status/replicas/2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var got Record
	if err := json.Unmarshal(e.Value, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.OK != rec.OK || got.Detail != rec.Detail {
		t.Errorf("record = %+v, want %+v", got, rec)
	}
	// Second publish (CAS against new revision) succeeds.
	rec2 := Record{OK: true, Detail: "HTTP 200", At: time.Now()}
	if err := pub(ctx, rec2); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	// And the store has no other replica keys (targeted key only).
	if _, err := st.Get(ctx, "/blocks/default/web/status/replicas/0"); err == nil {
		t.Error("unexpected replica 0 status key")
	}
}

// The package never opts into stale reads (documented contract).
func TestNoStaleReads(t *testing.T) {
	if bad := store.AssertNoStaleReads("."); len(bad) > 0 {
		t.Errorf("forbidden stale reads in health package: %v", bad)
	}
}
