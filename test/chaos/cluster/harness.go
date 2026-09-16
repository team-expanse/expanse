package chaos

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/expanse/expanse/internal/cluster/lease"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
	raft "github.com/hashicorp/raft"
)

// Harness is an in-process n-node raftstore cluster with fault injection:
// partitions and packet loss via the shared faultNet (wired through each
// node's raft.StreamLayer), and log-fsync latency via WrapLogStore.
//
// Formation mirrors the raftstore test cluster: node 0 bootstraps, the
// rest are added as voters by the leader. No forwarding endpoints: writers
// address the leader directly and retry through elections.
type Harness struct {
	t     *testing.T
	n     int
	nodes []*raftstore.Store
	dirs  []string
	ports []int
	live  []bool
	mu    sync.Mutex

	net   *faultNet
	slow  *slowLogStore
	delay atomic.Int64 // log-fsync latency, ns

	peerKeys map[int]wgtypes.Key // per-node WG private keys (chaosnet.go)
}

// NewHarness starts an n-node cluster and waits for a leader.
func NewHarness(t *testing.T, n int) *Harness {
	t.Helper()
	h := &Harness{
		t:     t,
		n:     n,
		net:   newFaultNet(),
		nodes: make([]*raftstore.Store, n),
		dirs:  make([]string, n),
		ports: make([]int, n),
		live:  make([]bool, n),
	}
	for i := 0; i < n; i++ {
		h.dirs[i] = t.TempDir()
		h.ports[i] = freePort(t)
	}
	h.startNode(0, true)
	h.WaitLeader(0, 15*time.Second)
	for i := 1; i < n; i++ {
		h.startNode(i, false)
	}
	lead := h.Leader()
	if lead == nil {
		t.Fatal("no leader after bootstrap")
	}
	for i := 1; i < n; i++ {
		if err := lead.AddVoter(h.nodeID(i), h.raftAddr(i)); err != nil {
			t.Fatalf("AddVoter n%d: %v", i, err)
		}
	}
	for i := 0; i < n; i++ {
		h.WaitLeader(i, 15*time.Second)
	}
	t.Cleanup(h.Close)
	return h
}

func (h *Harness) nodeID(i int) string { return fmt.Sprintf("n%d", i) }

func (h *Harness) raftAddr(i int) string { return fmt.Sprintf("127.0.0.1:%d", h.ports[i]) }

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("grab port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func (h *Harness) startNode(i int, bootstrap bool) {
	h.t.Helper()
	// Fresh stream layer per incarnation: raft's NetworkTransport.Close
	// closes the stream listener, so a restarted node re-listens.
	ln, err := net.Listen("tcp", h.raftAddr(i))
	if err != nil {
		h.t.Fatalf("listen n%d: %v", i, err)
	}
	stream := &chaosStream{Listener: ln, net: h.net, src: i, resolve: h.resolveAddr}
	var wrap func(raft.LogStore) raft.LogStore
	if h.slow == nil {
		h.slow = &slowLogStore{delay: func() time.Duration { return time.Duration(h.delay.Load()) }}
	}
	slow := h.slow
	wrap = func(l raft.LogStore) raft.LogStore { return &slowLogStore{LogStore: l, delay: slow.delay} }
	s, err := raftstore.Open(raftstore.Config{
		NodeID:       h.nodeID(i),
		BindAddr:     h.raftAddr(i),
		DataDir:      h.dirs[i],
		Bootstrap:    bootstrap,
		StreamLayer:  stream,
		WrapLogStore: wrap,
	})
	if err != nil {
		h.t.Fatalf("open n%d: %v", i, err)
	}
	h.mu.Lock()
	h.nodes[i] = s
	h.live[i] = true
	h.mu.Unlock()
}

// resolveAddr maps a raft address to a node index (-1 = unknown).
func (h *Harness) resolveAddr(addr string) int {
	for i := range h.ports {
		if h.raftAddr(i) == addr {
			return i
		}
	}
	return -1
}

// Node returns the live store of node i.
func (h *Harness) Node(i int) *raftstore.Store {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nodes[i]
}

// Live reports whether node i is running.
func (h *Harness) Live(i int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.live[i]
}

// Leader returns the current leader store, or nil.
func (h *Harness) Leader() *raftstore.Store {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.nodes {
		if s != nil && s.IsLeader() {
			return s
		}
	}
	return nil
}

// LeaderIndex returns the leader's node index, or -1.
func (h *Harness) LeaderIndex() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, s := range h.nodes {
		if s != nil && s.IsLeader() {
			return i
		}
	}
	return -1
}

// WaitLeader blocks until node i reports a leader.
func (h *Harness) WaitLeader(i int, timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s := h.Node(i); s != nil && s.Leader() != "" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.t.Fatalf("n%d: no leader within %v", i, timeout)
}

// Kill crash-stops node i (data dir preserved for Restart). The graceful
// Close approximates kill -9 in-process; real signal crashes are covered
// by the VM node-loss test.
func (h *Harness) Kill(i int) {
	h.t.Helper()
	h.mu.Lock()
	s := h.nodes[i]
	h.live[i] = false
	h.mu.Unlock()
	if s != nil {
		_ = s.Close()
	}
	// Drop its dialed conns with the filter (the store closed them with
	// its transport; clear the filter's tracking).
	h.net.mu.Lock()
	delete(h.net.conns, i)
	h.net.mu.Unlock()
}

// Restart revives node i from its original data dir and raft address.
func (h *Harness) Restart(i int) {
	h.t.Helper()
	h.startNode(i, false)
	h.WaitLeader(i, 15*time.Second)
}

// Partition splits the cluster into the given groups (symmetric).
func (h *Harness) Partition(groups ...[]int) {
	h.t.Helper()
	h.net.setPartition(groups)
}

// ClearPartition heals any partition.
func (h *Harness) ClearPartition() { h.net.clearPartition() }

// Loss sets the simulated dial-loss probability (also severs existing
// flows so new dials hit the filter).
func (h *Harness) Loss(p float64) { h.net.setLoss(p) }

// LogLatency sets the injected fsync latency (0 = none).
func (h *Harness) LogLatency(d time.Duration) { h.delay.Store(int64(d)) }

// LeaseManager returns a lease manager backed by node i, with an optional
// clock override (clock-skew scenario; nil = wall clock).
func (h *Harness) LeaseManager(i int, clock func() time.Time) *lease.Manager {
	m := lease.NewManager(h.Node(i), h.nodeID(i))
	if clock != nil {
		m = m.WithClock(clock)
	}
	return m
}

// Put writes through the current leader (retrying across elections).
// Fails the test only on ctx cancellation.
func (h *Harness) Put(ctx context.Context, k store.Key, v []byte) (store.Revision, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if lead := h.Leader(); lead != nil {
			rev, err := lead.Put(ctx, k, v)
			if err == nil {
				return rev, nil
			}
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Get reads a key linearizably through the current leader.
func (h *Harness) Get(ctx context.Context, k store.Key) (*store.Entry, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if lead := h.Leader(); lead != nil {
			e, err := lead.Get(ctx, k)
			if err == nil {
				return e, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// StateHashes returns each live node's FSM hash ("-" when down).
func (h *Harness) StateHashes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, h.n)
	for i, s := range h.nodes {
		if s != nil && h.live[i] {
			out[i] = fmt.Sprintf("%x", s.StateHash())
		} else {
			out[i] = "-"
		}
	}
	return out
}

// WaitConverged waits until some node is leader and every live node's FSM
// hash matches the leader's (post-heal convergence).
func (h *Harness) WaitConverged(timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		li := h.LeaderIndex()
		if li >= 0 {
			hashes := h.StateHashes()
			lead := hashes[li]
			if lead != "" {
				same := true
				for i, hash := range hashes {
					if h.Live(i) && hash != lead {
						same = false
						break
					}
				}
				if same {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("cluster did not converge within %v (hashes=%v)", timeout, h.StateHashes())
}

// Writer hammers puts through the leader, recording every acked write so
// the scenario can verify none of them were lost.
type Writer struct {
	h        *Harness
	ops      atomic.Int64
	mu       sync.Mutex
	acked    map[store.Key]acked
	seq      int
	stopOnce sync.Once
	stopped  chan struct{}
}

type acked struct {
	val string
	rev store.Revision
}

// StartWriter launches a background writer under prefix /chaos/.
func (h *Harness) StartWriter(ctx context.Context) *Writer {
	w := &Writer{h: h, acked: make(map[store.Key]acked), stopped: make(chan struct{})}
	go w.run(ctx)
	return w
}

func (w *Writer) run(ctx context.Context) {
	defer close(w.stopped)
	for {
		if ctx.Err() != nil {
			return
		}
		w.mu.Lock()
		w.seq++
		k := store.Key(fmt.Sprintf("/chaos/k%d", w.seq))
		v := fmt.Sprintf("v%d", w.seq)
		w.mu.Unlock()
		rev, err := w.h.Put(ctx, k, []byte(v))
		if err != nil {
			return // ctx canceled
		}
		w.mu.Lock()
		w.acked[k] = acked{val: v, rev: rev}
		w.mu.Unlock()
		w.ops.Add(1)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// Ops reports the number of acked writes so far.
func (w *Writer) Ops() int64 { return w.ops.Load() }

// AckedCount returns the size of the acked set (writer must be stopped).
func (w *Writer) AckedCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.acked)
}

// VerifyAcked reads every acked write back through the leader: present,
// same value, revision not lower. Any regression is data loss (§6
// random-kill invariant).
func (w *Writer) VerifyAcked(ctx context.Context) error {
	w.mu.Lock()
	snapshot := make(map[store.Key]acked, len(w.acked))
	for k, a := range w.acked {
		snapshot[k] = a
	}
	w.mu.Unlock()
	for k, a := range snapshot {
		e, err := w.h.Get(ctx, k)
		if err != nil {
			return fmt.Errorf("verify %s: %v", k, err)
		}
		if e == nil {
			return fmt.Errorf("verify %s: acked write missing", k)
		}
		if string(e.Value) != a.val {
			return fmt.Errorf("verify %s: value %q != acked %q", k, e.Value, a.val)
		}
		if e.Revision < a.rev {
			return fmt.Errorf("verify %s: revision %d regressed below acked %d", k, e.Revision, a.rev)
		}
	}
	return nil
}

// Stop ends the writer loop and waits for it.
func (w *Writer) Stop() {
	w.stopOnce.Do(func() {})
	<-w.stopped
}

// RandomLiveNode returns the index of a random running node.
func (h *Harness) RandomLiveNode() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	var live []int
	for i, ok := range h.live {
		if ok {
			live = append(live, i)
		}
	}
	if len(live) == 0 {
		return -1
	}
	return live[rand.Intn(len(live))]
}

// snapshotDir is unused today but keeps TempDir layout obvious for
// debugging: raft files live under <DataDir>/raft.
func (h *Harness) raftDir(i int) string { return filepath.Join(h.dirs[i], "raft") }

// Close shuts down every live node (idempotent; used by t.Cleanup).
func (h *Harness) Close() {
	h.mu.Lock()
	nodes := append([]*raftstore.Store(nil), h.nodes...)
	live := append([]bool(nil), h.live...)
	h.mu.Unlock()
	for i, s := range nodes {
		if s != nil && live[i] {
			_ = s.Close()
		}
	}
	h.ClearPartition()
	h.LogLatency(0)
}
