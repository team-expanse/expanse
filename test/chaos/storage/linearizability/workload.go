package linearizability

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/expanse/expanse/test/chaos/exvol"
)

// Recorder collects the concurrent history plus any read that returned
// bytes no client ever sent.
type Recorder struct {
	start time.Time

	mu         sync.Mutex
	ops        []Op
	violations []string
	nextID     int
}

func NewRecorder() *Recorder { return &Recorder{start: time.Now()} }

func (r *Recorder) now() int64 { return int64(time.Since(r.start)) }

func (r *Recorder) add(o Op) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	o.ID = r.nextID
	r.ops = append(r.ops, o)
}

func (r *Recorder) violate(format string, a ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.violations = append(r.violations, fmt.Sprintf(format, a...))
}

// History returns the recorded ops and violations so far.
func (r *Recorder) History() ([]Op, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Op(nil), r.ops...), append([]string(nil), r.violations...)
}

// keyPool hands out blocks so each register sees a bounded number of ops:
// exact checking cost stays flat however long the run is.
type keyPool struct {
	mu        sync.Mutex
	active    []int
	left      []int
	next      int
	max       int
	opsPerKey int
}

func newKeyPool(window, opsPerKey, maxKeys int) *keyPool {
	p := &keyPool{active: make([]int, window), left: make([]int, window), max: maxKeys, opsPerKey: opsPerKey}
	for i := range p.active {
		p.active[i], p.left[i] = i, opsPerKey
	}
	p.next = window
	return p
}

// pick returns a live key, retiring it after opsPerKey uses. ok is false
// once the volume has no fresh blocks left.
func (p *keyPool) pick(rng *rand.Rand) (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := rng.Intn(len(p.active))
	k := p.active[i]
	if p.left[i]--; p.left[i] <= 0 {
		if p.next >= p.max {
			return 0, false
		}
		p.active[i], p.left[i] = p.next, p.opsPerKey
		p.next++
	}
	return k, true
}

// settledKeys counts the low blocks that no client will touch again: every
// block below the lowest hot one, less a margin for operations still in flight.
func (p *keyPool) settledKeys(margin int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	lowest := p.active[0]
	for _, k := range p.active {
		lowest = min(lowest, k)
	}
	return max(lowest-margin, 0)
}

// usedKeys is how many distinct blocks have been handed out.
func (p *keyPool) usedKeys() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.next
}

// Workload drives concurrent clients against one volume.
type Workload struct {
	C      *exvol.Cluster
	Vol    string
	Rec    *Recorder
	Pool   *keyPool
	tokens atomic.Uint64
	nodes  []string

	lastReadErr atomic.Value // string: the most recent failed read, for diagnostics
}

func NewWorkload(c *exvol.Cluster, vol string, pool *keyPool, rec *Recorder, nodes []string) *Workload {
	return &Workload{C: c, Vol: vol, Rec: rec, Pool: pool, nodes: nodes}
}

// target is where a client sends a request: usually the primary it
// believes in, sometimes a random node (stale view / deposed primary).
func (w *Workload) target(rng *rand.Rand) *exvol.Node {
	if n := w.C.Node(w.C.Status(w.Vol).Primary); n != nil && rng.Intn(100) >= 15 {
		return n
	}
	return w.C.Node(w.nodes[rng.Intn(len(w.nodes))])
}

// Run loops one client until stop is closed. Returns an error if the
// volume ran out of fresh blocks.
func (w *Workload) Run(client int, stop <-chan struct{}) error {
	rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(client)))
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		key, ok := w.Pool.pick(rng)
		if !ok {
			return fmt.Errorf("volume out of fresh blocks; raise the volume size")
		}
		n := w.target(rng)
		if rng.Intn(100) < 60 {
			w.write(client, n, key)
		} else {
			_ = w.read(client, n, key)
		}
		time.Sleep(time.Duration(rng.Intn(20)) * time.Millisecond) // ~400 ops/s across all clients
	}
}

func (w *Workload) write(client int, n *exvol.Node, key int) {
	tok := w.tokens.Add(1)
	op := Op{Client: client, Key: key, Kind: Write, Value: tok, Call: w.Rec.now()}
	written := false // once the write step returned, a later flush error is NOT "no effect"
	err := w.serve(n, func() error {
		if err := n.Runtime().WriteOp(w.Vol, int64(key)*BlockSize, EncodeBlock(key, tok)); err != nil {
			return err
		}
		written = true
		return n.Runtime().FlushOp(w.Vol)
	})
	op.Return = w.Rec.now()
	switch {
	case err == nil:
	case !written && hadNoEffect(err):
		return // rejected before touching any replica: not part of the history
	default:
		op.Indeterminate = true
	}
	w.Rec.add(op)
}

// read reports whether the read succeeded (and was recorded).
func (w *Workload) read(client int, n *exvol.Node, key int) bool {
	op := Op{Client: client, Key: key, Kind: Read, Call: w.Rec.now()}
	buf := make([]byte, BlockSize)
	if err := w.serve(n, func() error { return n.Runtime().ReadOp(w.Vol, int64(key)*BlockSize, buf) }); err != nil {
		w.lastReadErr.Store(fmt.Sprintf("%s: %v", n.ID, err))
		return false // a failed read observed nothing
	}
	op.Return = w.Rec.now()
	tok, err := DecodeBlock(key, buf)
	if err != nil {
		w.Rec.violate("client %d read key %d from %s: %v", client, key, n.ID, err)
		return false
	}
	op.Value = tok
	w.Rec.add(op)
	return true
}

// serve runs fn "inside" node n: a paused (SIGSTOPped) node serves nothing.
func (w *Workload) serve(n *exvol.Node, fn func() error) error {
	w.C.Faults.WaitRunning(n.Idx)
	return fn()
}

// hadNoEffect recognises errors raised before any replica saw the write.
func hadNoEffect(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "not primary for") ||
		(strings.Contains(msg, "exvol.device.write") && strings.Contains(msg, "lease lost")) ||
		(strings.Contains(msg, "exvol.device.write") && strings.Contains(msg, "device closed"))
}
