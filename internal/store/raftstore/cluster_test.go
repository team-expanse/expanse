package raftstore_test

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
)

// TestCluster is an in-process n-node Raft cluster with real TCP raft
// transports and per-node internal gRPC forward endpoints on loopback
// (T07 harness; used by T06 forwarding tests and the N=3 conformance run).
//
// Formation: node 0 bootstraps a single-voter cluster; nodes 1..n-1 open
// unbootstrapped and are added as voters through the leader's AddVoter.
type TestCluster struct {
	t      *testing.T
	Nodes  []*raftstore.Store
	dirs   []string
	ports  []int // raft ports, stable across restarts
	api    []int // api ports, picked on first start and kept for restarts
	apiLn  []net.Listener
	srvs   []*grpc.Server
	fwd    []*raftstore.GRPCForwarder
	dead   []bool
	deadMu sync.Mutex // guards dead and api
}

// freePort grabs an ephemeral TCP port. Races are theoretically possible;
// they are vanishingly rare on loopback and the subsequent listen failure
// is loud.
var freePort = func(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("grab port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// NewTestCluster starts an n-node cluster and waits for a leader.
func NewTestCluster(t *testing.T, n int) *TestCluster {
	t.Helper()
	c := &TestCluster{t: t}
	for i := 0; i < n; i++ {
		c.dirs = append(c.dirs, t.TempDir())
		c.ports = append(c.ports, freePort(t))
		c.api = append(c.api, 0)
		c.dead = append(c.dead, false)
	}
	c.start(0, true) // bootstrap node 0
	c.waitForLeader(0, 10*time.Second)
	for i := 1; i < n; i++ {
		c.start(i, false)
	}
	// Register voters 1..n-1 through the leader.
	for i := 1; i < n; i++ {
		if err := c.Nodes[0].AddVoter(c.nodeID(i), c.raftAddr(i)); err != nil {
			t.Fatalf("AddVoter %d: %v", i, err)
		}
	}
	for i := 0; i < n; i++ {
		c.waitForLeader(i, 10*time.Second)
	}
	t.Cleanup(c.Close)
	return c
}

func (c *TestCluster) nodeID(i int) string { return fmt.Sprintf("n%d", i) }
func (c *TestCluster) raftAddr(i int) string {
	return fmt.Sprintf("127.0.0.1:%d", c.ports[i])
}

// apiAddr is node i's internal endpoint; port 0 until its first start.
func (c *TestCluster) apiAddr(i int) string {
	c.deadMu.Lock()
	defer c.deadMu.Unlock()
	return fmt.Sprintf("127.0.0.1:%d", c.api[i])
}

// start opens node i's store and its internal gRPC forward endpoint.
func (c *TestCluster) start(i int, bootstrap bool) {
	c.t.Helper()
	s, err := raftstore.Open(raftstore.Config{
		NodeID:    c.nodeID(i),
		BindAddr:  c.raftAddr(i),
		DataDir:   c.dirs[i],
		Bootstrap: bootstrap,
	})
	if err != nil {
		c.t.Fatalf("open node %d: %v", i, err)
	}
	c.Nodes = ensureNode(c.Nodes, i, s)

	// Internal gRPC endpoint (forwarding). Plaintext: mTLS lands in T10.
	ln, err := net.Listen("tcp", c.apiAddr(i))
	if err != nil {
		c.t.Fatalf("api listen %d: %v", i, err)
	}
	c.deadMu.Lock()
	c.api[i] = ln.Addr().(*net.TCPAddr).Port
	c.deadMu.Unlock()
	srv := grpc.NewServer(grpc.Creds(insecure.NewCredentials()))
	pb.RegisterInternalStoreServiceServer(srv, raftstore.NewForwardServer(s))
	go srv.Serve(ln) //nolint:errcheck — test server

	// Every node can resolve every peer's internal endpoint; the forwarder
	// asks the node's raft state for the current leader address.
	fwd := raftstore.NewGRPCForwarder(
		func() string { return s.Leader() },
		func(raftAddr string) (string, bool) {
			for j := range c.ports {
				if c.raftAddr(j) == raftAddr && !strings.HasSuffix(c.apiAddr(j), ":0") {
					return c.apiAddr(j), true
				}
			}
			return "", false
		})
	s.SetForwarder(fwd.Forward)
	s.SetReadForwarder(fwd)
	s.SetMembershipForwarder(fwd)

	c.apiLn = ensureListener(c.apiLn, i, ln)
	c.srvs = ensureServer(c.srvs, i, srv)
	c.fwd = ensureForwarder(c.fwd, i, fwd)
	c.deadMu.Lock()
	c.dead[i] = false
	c.deadMu.Unlock()
}

func ensureNode(n []*raftstore.Store, i int, s *raftstore.Store) []*raftstore.Store {
	for len(n) <= i {
		n = append(n, nil)
	}
	n[i] = s
	return n
}

func ensureListener(n []net.Listener, i int, l net.Listener) []net.Listener {
	for len(n) <= i {
		n = append(n, nil)
	}
	n[i] = l
	return n
}

func ensureServer(n []*grpc.Server, i int, s *grpc.Server) []*grpc.Server {
	for len(n) <= i {
		n = append(n, nil)
	}
	n[i] = s
	return n
}

func ensureForwarder(n []*raftstore.GRPCForwarder, i int, f *raftstore.GRPCForwarder) []*raftstore.GRPCForwarder {
	for len(n) <= i {
		n = append(n, nil)
	}
	n[i] = f
	return n
}

// waitForLeader blocks until node i reports a leader.
func (c *TestCluster) waitForLeader(i int, timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.Nodes[i].Leader() != "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatalf("node %d: no leader within %v", i, timeout)
}

// Leader returns the current leader node, or nil.
func (c *TestCluster) Leader() *raftstore.Store {
	for _, n := range c.Nodes {
		if n != nil && n.IsLeader() {
			return n
		}
	}
	return nil
}

// Follower returns any non-leader node.
func (c *TestCluster) Follower() *raftstore.Store {
	c.t.Helper()
	for _, n := range c.Nodes {
		if n != nil && !n.IsLeader() && n.Leader() != "" {
			return n
		}
	}
	c.t.Fatal("no follower with a leader")
	return nil
}

// Kill shuts down node i but preserves its data dir (crash simulation).
func (c *TestCluster) Kill(i int) {
	c.t.Helper()
	if s := c.Nodes[i]; s != nil {
		_ = s.Close()
	}
	if c.srvs[i] != nil {
		c.srvs[i].Stop()
	}
	if c.fwd[i] != nil {
		_ = c.fwd[i].Close()
	}
	c.deadMu.Lock()
	c.dead[i] = true
	c.deadMu.Unlock()
}

// Restart revives node i with its original data dir and raft address.
func (c *TestCluster) Restart(i int) {
	c.t.Helper()
	c.start(i, false)
	c.waitForLeader(i, 10*time.Second)
}

// Close shuts down everything (idempotent).
func (c *TestCluster) Close() {
	for i := range c.Nodes {
		if s := c.Nodes[i]; s != nil {
			_ = s.Close()
			c.Nodes[i] = nil
		}
	}
	for _, s := range c.srvs {
		if s != nil {
			s.Stop()
		}
	}
	for _, f := range c.fwd {
		if f != nil {
			_ = f.Close()
		}
	}
}

// Dir returns node i's data dir (for log-forensics assertions).
func (c *TestCluster) Dir(i int) string { return filepath.Join(c.dirs[i], "raft") }
