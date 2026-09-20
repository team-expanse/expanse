package primary

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slowLocal is a LocalWriter whose WriteAt holds the write open so a test
// can observe the window between sequence assignment and local apply.
type slowLocal struct {
	entered chan struct{}
	release chan struct{}
	applied atomic.Uint64
	once    sync.Once
}

func (s *slowLocal) WriteAt(p []byte, off int64) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	s.applied.Add(1)
	return nil
}
func (s *slowLocal) Flush() error                            { return nil }
func (s *slowLocal) ReadAt(p []byte, off int64) (int, error) { return len(p), nil }

// A snapshot named for seq S must contain every op <= S. A write holds its
// sequence number before its bytes are applied, so AtSeq must wait for it.
func TestAtSeqNeverSeesAssignedButUnappliedWrite(t *testing.T) {
	l := &slowLocal{entered: make(chan struct{}), release: make(chan struct{})}
	c := New("vol-test", 1, l, nil, &fakeLease{valid: true}, time.Second)

	writeDone := make(chan error, 1)
	go func() { writeDone <- c.Write([]byte("x"), 0) }()
	<-l.entered // seq 1 assigned, local write in flight

	gotSeq, gotApplied := make(chan uint64, 1), make(chan uint64, 1)
	go func() {
		_ = c.AtSeq(func(seq uint64) error {
			gotSeq <- seq
			gotApplied <- l.applied.Load()
			return nil
		})
	}()
	select {
	case <-gotSeq:
		t.Fatal("AtSeq ran while a write was between sequence assignment and local apply")
	case <-time.After(100 * time.Millisecond):
	}
	close(l.release)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if seq, applied := <-gotSeq, <-gotApplied; seq != 1 || applied != 1 {
		t.Fatalf("AtSeq saw seq=%d applied=%d, want both 1", seq, applied)
	}
}

// A demoted primary's coordinator must not leave pump goroutines behind.
func TestCloseRetiresEveryReplicaPump(t *testing.T) {
	size := int64(1 << 20)
	a, b := newTestReplica(t, "n1", size), newTestReplica(t, "n2", size)
	c, _, _ := newTestPrimary(t, size, []*testReplica{a, b}, &fakeLease{valid: true}, time.Second)
	if err := c.Write([]byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	c.Close()
	for id, st := range c.replicas {
		select {
		case <-st.exited:
		case <-time.After(2 * time.Second):
			t.Fatalf("pump for %s still running after Close", id)
		}
	}
}

// A write that reaches a retired primary (its device server is still draining
// connections) must be refused up front, and say so: the local writer is
// about to be closed, and "file already closed" is not an answer a client can act on.
func TestWriteAfterCloseIsAClearRefusal(t *testing.T) {
	c := New("vol-test", 1, okLocal{}, nil, &fakeLease{valid: true}, time.Second)
	c.Close()
	for name, op := range map[string]func() error{
		"write": func() error { return c.Write([]byte("x"), 0) },
		"flush": c.Flush,
	} {
		err := op()
		if err == nil {
			t.Fatalf("%s on a closed primary was accepted", name)
		}
		if msg := err.Error(); !strings.Contains(msg, "not primary") {
			t.Errorf("%s: err = %q, want a clear 'not primary' refusal", name, msg)
		}
	}
}

// okLocal is a LocalWriter that always succeeds.
type okLocal struct{}

func (okLocal) WriteAt(p []byte, off int64) error       { return nil }
func (okLocal) Flush() error                            { return nil }
func (okLocal) ReadAt(p []byte, off int64) (int, error) { return len(p), nil }
