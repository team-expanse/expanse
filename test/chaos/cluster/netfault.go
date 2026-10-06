// Package chaos implements the Phase 03 §6 chaos suite: six fault-injection
// scenarios driven against an in-process n-node raftstore cluster, plus the
// split-brain invariant checker (§6 "the single most important invariant
// checker"): a 100 ms poller that asserts, continuously, that no lease is
// ever held by more than one node.
//
// Scenarios (spec table, §6 463–486):
//
//	random-kill    kill/restart a random node   → no lost acked writes,
//	                                              monotonic revisions
//	partition-storm random partitions           → never two lease holders,
//	                                              no divergence after heal
//	clock-skew     ±offsets and rate drift      → never two lease holders
//	slow-disk      injected fsync latency       → available, just slower
//	                                              (no corruption)
//	packet-loss    5/20/50 % dial loss          → converges; leader may
//	                                              flap, no split-brain
//	leader-churn   forced leadership transfers  → writes stay linearizable
//
// Network faults (partitions, packet loss) are injected through the
// raftstore.Config.StreamLayer test hook: every dial and every dialed
// connection passes through a shared filter that this package controls.
// Disk faults are injected through raftstore.Config.WrapLogStore, wrapping
// the bolt log store (each StoreLogs call models one fsync). Clock skew is
// injected through lease.Manager.WithClock (the grant/renew timestamps),
// while all timing loops stay on the monotonic clock — that separation is
// precisely what the skew scenario exercises.
//
// The scenarios are Go tests: short-duration by default (CHAOS_DURATION,
// 20 s) so the regular suite stays fast, and RUN_CHAOS=1 scales every
// scenario to its full 5-minute CI-nightly length.
package chaos

import (
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/expanse/expanse/internal/errors"
	raft "github.com/hashicorp/raft"
)

// faultNet is the shared network-fault filter. Every node's raft transport
// dials and accepts through it, so a partition closes existing crossing
// connections too (a real partition drops established flows, not just new
// dials).
type faultNet struct {
	mu sync.Mutex
	// group maps node index → partition group. Empty/absent map = all in
	// one group (no partition).
	group map[int]int
	// loss is the probability that a dial "fails" (simulated packet loss
	// severe enough to kill the flow setup).
	loss float64
	// conns tracks live dialed connections per source node so a partition
	// change can sever the ones crossing the split.
	conns map[int]map[net.Conn]struct{}
}

func newFaultNet() *faultNet {
	return &faultNet{conns: make(map[int]map[net.Conn]struct{})}
}

func (f *faultNet) setPartition(groups [][]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.group = make(map[int]int)
	for g, nodes := range groups {
		for _, n := range nodes {
			f.group[n] = g
		}
	}
	// Sever every connection crossing a group boundary. Existing pooled
	// conns would otherwise keep the "partitioned" peers talking.
	for src, conns := range f.conns {
		for c := range conns {
			if f.crossingLocked(src, remoteIdx(c)) {
				_ = c.Close()
			}
		}
	}
}

func (f *faultNet) clearPartition() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.group = nil
}

func (f *faultNet) setLoss(p float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loss = p
	// Force every flow to re-establish through the new loss filter.
	for _, conns := range f.conns {
		for c := range conns {
			_ = c.Close()
		}
	}
}

// sameGroupLocked reports whether src and dst belong to one partition
// group (dial allowed). dst -1 = unknown address → allow.
func (f *faultNet) sameGroupLocked(src, dst int) bool {
	if len(f.group) == 0 || dst < 0 {
		return true
	}
	return f.group[src] == f.group[dst]
}

func (f *faultNet) crossingLocked(src, dst int) bool {
	return dst >= 0 && !f.sameGroupLocked(src, dst)
}

// remoteIdx identifies the destination node of a dialed conn. The wrapper
// records the resolved index on the conn at dial time (connAddr). Unknown
// destinations (harness restarts, probes) read as -1 → allowed.
type taggedConn struct {
	net.Conn
	dst int
}

func remoteIdx(c net.Conn) int {
	if t, ok := c.(*taggedConn); ok {
		return t.dst
	}
	return -1
}

// denyLocked decides whether src may dial dst now; may drop for loss.
func (f *faultNet) denyLocked(src, dst int) error {
	if f.crossingLocked(src, dst) {
		return errors.New(errors.KindUnavailable, "chaos.partition", fmt.Sprintf("simulated partition: n%d to n%d", src, dst))
	}
	if f.loss > 0 && rand.Float64() < f.loss {
		return errors.New(errors.KindUnavailable, "chaos.loss", fmt.Sprintf("simulated packet loss: n%d to n%d", src, dst))
	}
	return nil
}

func (f *faultNet) track(src int, c net.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conns[src] == nil {
		f.conns[src] = make(map[net.Conn]struct{})
	}
	f.conns[src][c] = struct{}{}
}

// chaosStream is a per-node raft.StreamLayer whose dials pass the shared
// faultNet filter. It embeds a plain TCP listener on the node's raft port.
type chaosStream struct {
	net.Listener
	net *faultNet
	src int
	// resolve maps a peer raft address to its node index (-1 if unknown).
	resolve func(addr string) int
}

// Dial implements raft.StreamLayer.
func (s *chaosStream) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	dst := s.resolve(string(address))
	s.net.mu.Lock()
	err := s.net.denyLocked(s.src, dst)
	s.net.mu.Unlock()
	if err != nil {
		return nil, err
	}
	c, cerr := net.DialTimeout("tcp", string(address), timeout)
	if cerr != nil {
		return nil, cerr
	}
	tc := &taggedConn{Conn: c, dst: dst}
	s.net.track(s.src, tc)
	return tc, nil
}

// slowLogStore wraps the bolt log store, sleeping on every StoreLogs call
// to emulate injected fsync latency (the device-mapper delay of the spec,
// in-process). Close is forwarded via the embedded LogStore's Close if it
// has one; raftstore closes the raw bolt store itself either way.
type slowLogStore struct {
	raft.LogStore
	delay func() time.Duration
}

func (l *slowLogStore) StoreLogs(entries []*raft.Log) error {
	if d := l.delay(); d > 0 {
		time.Sleep(d)
	}
	return l.LogStore.StoreLogs(entries)
}
