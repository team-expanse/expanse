package chaosstorage

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/expanse/expanse/test/chaos/exvol"
	"github.com/expanse/expanse/test/chaos/storage/linearizability"
)

const (
	blockSize       = linearizability.BlockSize
	blocksPerWriter = 32 // blocks each writer cycles through (so blocks are overwritten)
)

// Load runs concurrent writers, each owning a private range of blocks,
// and records every write's latency and outcome plus a ledger of what
// was sent and acked. A quarter of acked writes are read straight back.
type Load struct {
	C      *exvol.Cluster
	Vol    string
	Ledger Ledger

	start  time.Time
	tokens atomic.Uint64

	mu      sync.Mutex
	samples Samples
	bad     []string // reads that returned the wrong data
}

func NewLoad(c *exvol.Cluster, vol string) *Load {
	return &Load{C: c, Vol: vol, start: time.Now()}
}

// Since is the run clock the samples are stamped with.
func (l *Load) Since() time.Duration { return time.Since(l.start) }

// Samples returns a copy of every write outcome so far.
func (l *Load) Samples() Samples {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append(Samples(nil), l.samples...)
}

// Violations lists read-backs that disagreed with an acked write.
func (l *Load) Violations() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.bad...)
}

// Run starts `writers` clients pausing up to `think` between writes and
// returns an idempotent function that stops them and waits for them.
func (l *Load) Run(writers int, think time.Duration) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			l.writer(w, think, done)
		}(w)
	}
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		wg.Wait()
	}
}

func (l *Load) writer(w int, think time.Duration, done <-chan struct{}) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(w)))
	for i := 0; ; i++ {
		select {
		case <-done:
			return
		default:
		}
		block := w*blocksPerWriter + i%blocksPerWriter
		if tok, ok := l.write(block); ok && rng.Intn(4) == 0 {
			l.readBack(block, tok)
		}
		if think > 0 {
			time.Sleep(time.Duration(rng.Int63n(int64(think))))
		}
	}
}

// write sends one durable (write+flush) block write and records it.
func (l *Load) write(block int) (tok uint64, acked bool) {
	tok = l.tokens.Add(1)
	l.Ledger.Attempt(block, tok)
	at := l.Since()
	err := l.sendDurable(block, tok)
	l.record(Sample{At: at, Dur: l.Since() - at, Err: err})
	if err == nil {
		l.Ledger.Ack(block, tok)
	}
	return tok, err == nil
}

func (l *Load) sendDurable(block int, tok uint64) error {
	n := l.C.Node(l.C.Status(l.Vol).Primary)
	if n == nil || !n.Alive() {
		return fmt.Errorf("not primary for %s: elected node is down", l.Vol)
	}
	if err := n.Runtime().WriteOp(l.Vol, int64(block)*blockSize, linearizability.EncodeBlock(block, tok)); err != nil {
		return err
	}
	return n.Runtime().FlushOp(l.Vol)
}

// readBack verifies an acked write is visible; a failed read proves
// nothing (the node may be mid-failover) and is ignored.
func (l *Load) readBack(block int, want uint64) {
	n := l.C.Node(l.C.Status(l.Vol).Primary)
	if n == nil || !n.Alive() {
		return
	}
	buf := make([]byte, blockSize)
	if err := n.Runtime().ReadOp(l.Vol, int64(block)*blockSize, buf); err != nil {
		return
	}
	got, err := linearizability.DecodeBlock(block, buf)
	if err != nil || got != want {
		l.mu.Lock()
		l.bad = append(l.bad, fmt.Sprintf("block %d: acked token %d, read back %d (%v) from %s", block, want, got, err, n.ID))
		l.mu.Unlock()
	}
}

func (l *Load) record(s Sample) {
	l.mu.Lock()
	l.samples = append(l.samples, s)
	l.mu.Unlock()
}

// VerifyDurable reads every written block through the current primary
// (retrying while it settles) and checks it against the ledger.
func (l *Load) VerifyDurable(deadline time.Duration) []error {
	var errs []error
	stop := time.Now().Add(deadline)
	for _, block := range l.Ledger.Blocks() {
		got, err := l.readUntil(block, stop)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := l.Ledger.Check(block, got); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func (l *Load) readUntil(block int, stop time.Time) (uint64, error) {
	buf := make([]byte, blockSize)
	for {
		n := l.C.Node(l.C.Status(l.Vol).Primary)
		var err error
		if n == nil || !n.Alive() {
			err = fmt.Errorf("primary is down")
		} else if err = n.Runtime().ReadOp(l.Vol, int64(block)*blockSize, buf); err == nil {
			return linearizability.DecodeBlock(block, buf)
		}
		if time.Now().After(stop) {
			return 0, fmt.Errorf("block %d unreadable: %w", block, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
