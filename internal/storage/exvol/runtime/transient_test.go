package runtime

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/storage/exvol/recovery"
	"github.com/expanse/expanse/internal/storage/exvol/transport"
)

func TestTransientTrackerLatchesOnlyAfterTheWindow(t *testing.T) {
	tr := newTransientTracker(60 * time.Second)
	t0 := time.Now()
	if tr.note("v", t0) {
		t.Fatal("the first transient failure must not latch")
	}
	if tr.note("v", t0.Add(30*time.Second)) {
		t.Fatal("still inside the window")
	}
	if !tr.note("v", t0.Add(61*time.Second)) {
		t.Fatal("failures persisting past the window must latch")
	}
}

func TestTransientTrackerResetsOnSuccessAndOnGaps(t *testing.T) {
	tr := newTransientTracker(60 * time.Second)
	t0 := time.Now()
	tr.note("v", t0)
	tr.clear("v") // a successful recovery
	if tr.note("v", t0.Add(90*time.Second)) {
		t.Fatal("a cleared volume starts a fresh window")
	}
	tr.note("w", t0)
	if tr.note("w", t0.Add(10*time.Minute)) {
		t.Fatal("an old failure long past is not 'persisting': the window restarts")
	}
}

func TestTransientTrackerKeepsVolumesApart(t *testing.T) {
	tr := newTransientTracker(time.Second)
	t0 := time.Now()
	tr.note("a", t0)
	if tr.note("b", t0.Add(2*time.Second)) {
		t.Fatal("volume b's first failure must not inherit a's window")
	}
}

func TestClassifyFetchErr(t *testing.T) {
	var tr *recovery.TransportError
	if err := classifyFetchErr(io.EOF); !errors.As(err, &tr) {
		t.Errorf("a broken connection must be marked as transport, got %v", err)
	}
	for _, err := range []error{errors.New("crc"), &transport.ReplicaError{Msg: "no such op"}} {
		if got := classifyFetchErr(err); errors.As(got, &tr) {
			t.Errorf("%v answered: it must stay definitive", err)
		}
	}
}

// A peer that accepts the connection and never answers must not hold up
// recovery (and with it the volume): the probe is cut off at its deadline.
func TestQuerySeqWithinGivesUpOnASilentPeer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() //nolint:errcheck // held open, never answered
		}
	}()
	conn, err := transport.Dial(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := querySeqWithin(conn, "v", 150*time.Millisecond); err == nil {
		t.Fatal("a silent peer must yield an error")
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("probe took %v, want ~150ms", took)
	}
}
