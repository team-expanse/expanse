// Package exvol is an in-process chaos harness for the exvol volume
// runtime (Phase 06 T17.1). Real packages — runtime, primary,
// secondary, recovery, resync, localwrite, transport — over loopback
// TCP, with a file-backed fake zfs/zpool (fake-zfs.sh) instead of a
// real pool and one shared boltstore standing in for the raft log.
//
// Scenarios that each cost a 10-minute NixOS VM run (crash, restart,
// failover, oplog survival, zvol loss, divergence) run here in
// milliseconds, deterministically. The VM test (vol-durability.nix)
// remains the final integration gate.
//
// Crash model: cancelling a node's Run context is a daemon restart —
// stopAll uses Held.Abandon, which leaves the volume lease to expire
// naturally (≤ TTL) exactly like a dead process; the durable oplog and
// the zvol file survive on disk and are reloaded at startup. Zvol loss
// (crashed-pool txg) is simulated by wiping the node's fake pool dir —
// it holds the zvol file AND the oplog, one crash domain, matching
// production's zvol+oplog-in-pool layout.
package exvol

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/storage/exvol/runtime"
	"github.com/expanse/expanse/internal/storage/exvol/transport"
	"github.com/expanse/expanse/internal/storage/zfs"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

const pool = "volumes" // fake pool name (mirrors the VM test pool)

// Node is one harness node: a runtime over its own fake pool.
type Node struct {
	ID   string
	Idx  int    // 1-based position; names the loopback addresses
	Root string // tmpdir: bin/, data/, dev/, pool/ (zvol + oplog live here)
	Port int

	// mu guards RT/alive/cancelRun: Crash/Restart run concurrently with
	// client goroutines. Concurrent code must use Runtime()/Alive().
	mu        sync.Mutex
	RT        *runtime.Runtime
	alive     bool
	ip        string
	cancelRun context.CancelFunc
	done      chan struct{}
}

// Cluster is a set of nodes sharing one store (the raft-log stand-in)
// and a loopback mesh mapping.
type Cluster struct {
	T   *testing.T
	St  *boltstore.Store
	Ctx context.Context // cluster-wide context (nil → Background)

	// Faults is non-nil only for NewFaultCluster clusters.
	Faults *Faults

	Nodes   []*Node
	byID    map[string]*Node
	logSink *testWriter
	port    int
}

// NewCluster brings up nodes reconciling on a fast tick. One shared
// boltstore represents the replicated log every node reads/writes.
func NewCluster(t *testing.T, ids ...string) *Cluster {
	t.Helper()
	return newCluster(t, false, ids...)
}

// NewFaultCluster is NewCluster with the per-link fault layer (Faults)
// interposed on every replication connection and store view.
func NewFaultCluster(t *testing.T, ids ...string) *Cluster {
	t.Helper()
	return newCluster(t, true, ids...)
}

func newCluster(t *testing.T, faulted bool, ids ...string) *Cluster {
	t.Helper()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "raft.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := &Cluster{T: t, St: st, byID: map[string]*Node{}}
	// All nodes share one replication port (mirrors production, where
	// every node listens on PortExvol); each binds its own loopback IP
	// (127.0.0.<i> — the whole 127/8 is local), standing in for the
	// per-node mesh addresses.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c.port = ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	if faulted {
		c.Faults = newFaults()
		t.Cleanup(c.Faults.stop)
		for s := range ids {
			for d := range ids {
				if s != d {
					if err := c.Faults.addLink(s+1, d+1, c.port); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	for i, id := range ids {
		n := c.newNode(id, "", i+1)
		c.Nodes = append(c.Nodes, n)
		c.byID[id] = n
	}
	for _, n := range c.Nodes {
		c.start(n)
	}
	t.Cleanup(c.Stop)
	return c
}

func (c *Cluster) ctx() context.Context {
	if c.Ctx == nil {
		c.Ctx = context.Background()
	}
	return c.Ctx
}

// AddrOf resolves a node ID to its loopback replication address.
func (c *Cluster) AddrOf(nodeID string) (string, error) {
	n, ok := c.byID[nodeID]
	if !ok {
		return "", fmt.Errorf("no such node %q", nodeID)
	}
	return n.ip, nil // host only; the runtime appends the shared port
}

// Node returns a node by ID.
func (c *Cluster) Node(id string) *Node { return c.byID[id] }

// Stop cancels every node (daemon exit — leases expire, not yank).
func (c *Cluster) Stop() {
	if c.logSink != nil {
		c.logSink.mu.Lock()
		c.logSink.closed = true
		c.logSink.mu.Unlock()
	}
	for _, n := range c.Nodes {
		c.stopNode(n)
	}
}

// ProbeSeq is the controller's election probe (§4.3 4a): the node's
// last durable sequence for volID, read over the replication transport.
func (c *Cluster) ProbeSeq(volID, nodeID string) (uint64, error) {
	n := c.byID[nodeID]
	if n == nil {
		return 0, fmt.Errorf("no such node %q", nodeID)
	}
	ctx, cancel := context.WithTimeout(c.ctx(), time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, fmt.Sprintf("%s:%d", n.ip, n.Port))
	if err != nil {
		return 0, err
	}
	defer conn.Close() //nolint:errcheck
	rep, err := conn.QuerySeq(volID)
	if err != nil {
		return 0, err
	}
	return rep.GetLastSeq(), nil
}

// Alive reports whether the node's runtime is running (not crashed).
func (n *Node) Alive() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.alive
}

// Runtime returns the node's current runtime (replaced by Restart).
func (n *Node) Runtime() *runtime.Runtime {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.RT
}

// ZvolFile is the path of the node's zvol backing file for volID.
func (n *Node) ZvolFile(volID string) string {
	return filepath.Join(n.Root, "pool", "dev", pool, "volumes", volID)
}

// DeviceAttached reports whether this node's fake NBD device marker is
// currently published (mirrors production's /dev/exvol/<id> symlink,
// torn down by DetachNBD on demotion/delete/shutdown).
func (n *Node) DeviceAttached() bool {
	entries, err := os.ReadDir(filepath.Join(n.Root, "exvol"))
	if err != nil {
		return false
	}
	return len(entries) > 0
}

func (c *Cluster) stopNode(n *Node) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.RT != nil && n.alive {
		n.cancelRun()
		n.alive = false
	}
}

// newNode wires one node: fake zfs binaries, reserved port, runtime.
// If old is non-empty the node is rebuilt over that existing root
// (restart: zvol + durable oplog survive).
func (c *Cluster) newNode(id, oldRoot string, idx int) *Node {
	c.T.Helper()
	root := oldRoot
	if root == "" {
		root = c.T.TempDir()
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		c.T.Fatal(err)
	}
	for _, d := range []string{"data", "dev", "pool"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			c.T.Fatal(err)
		}
	}

	// Fake zfs/zpool wrappers: bake FAKE_ZFS_ROOT in per node.
	// Resolved from this source file so other packages can use the harness.
	_, self, _, _ := goruntime.Caller(0)
	fake := filepath.Join(filepath.Dir(self), "fake-zfs.sh")
	if _, err := os.Stat(fake); err != nil {
		c.T.Fatalf("fake-zfs.sh not found next to harness: %v", err)
	}
	poolRoot := filepath.Join(root, "pool")
	// Copy the shared emulator into the node's bin with exec rights.
	nodeFake := filepath.Join(bin, "fake-zfs.sh")
	raw, err := os.ReadFile(fake)
	if err != nil {
		c.T.Fatal(err)
	}
	if err := os.WriteFile(nodeFake, raw, 0o755); err != nil {
		c.T.Fatal(err)
	}
	for _, b := range []string{"zfs", "zpool"} {
		w := fmt.Sprintf("#!/bin/sh\nFAKE_ZFS_ROOT=%q exec %q %s \"$@\"\n",
			poolRoot, nodeFake, b)
		if err := os.WriteFile(filepath.Join(bin, b), []byte(w), 0o755); err != nil {
			c.T.Fatal(err)
		}
	}

	port := c.port
	ip := fmt.Sprintf("127.0.0.%d", idx)
	var st store.Store = c.St
	addrOf := c.AddrOf
	if c.Faults != nil {
		// Serve on the back address; peers reach us only through the proxies.
		ip = backIP(idx)
		st = &gatedStore{Store: c.St, f: c.Faults, idx: idx}
		addrOf = func(nodeID string) (string, error) {
			d, ok := c.byID[nodeID]
			if !ok {
				return "", fmt.Errorf("no such node %q", nodeID)
			}
			if d.Idx == idx {
				return ip, nil
			}
			return linkIP(idx, d.Idx), nil
		}
	}
	zexec := zfs.New()
	zexec.ZfsPath = filepath.Join(bin, "zfs")
	zexec.ZpoolPath = filepath.Join(bin, "zpool")

	rt := runtime.New(runtime.Options{
		NodeID:           id,
		St:               st,
		Pool:             pool,
		DataDir:          filepath.Join(root, "data"),
		Logger:           c.nodeLogger(id),
		AddrOf:           addrOf,
		Port:             port,
		ListenAddr:       ip + ":%d",
		LeaseTTL:         2 * time.Second,
		SnapshotInterval: 1 * time.Second,
		SnapshotKeep:     5,
		IsLeader:         func() bool { return true },
		ZvolDevBase:      filepath.Join(root, "pool", "dev"), // the fake nests dev/ under the pool
		ZFS:              zexec,
		AttachNBD: func(sock, volID, nbdDev string) error {
			// No kernel NBD here: publish a plain symlink marker.
			dir := filepath.Join(root, "exvol")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			tgt := filepath.Join(dir, filepath.Base(nbdDev))
			_ = os.Remove(tgt)
			return os.Symlink(sock, tgt)
		},
		DetachNBD: func(volID, nbdDev string) error {
			tgt := filepath.Join(root, "exvol", filepath.Base(nbdDev))
			_ = os.Remove(tgt)
			return nil
		},
		Tick: 200 * time.Millisecond,
	})
	return &Node{ID: id, Idx: idx, Root: root, Port: port, RT: rt, ip: ip}
}

// nodeLogger silences per-node logs unless the test is verbose.
func (c *Cluster) nodeLogger(id string) *slog.Logger {
	lvl := slog.LevelWarn
	if os.Getenv("EXVOL_HARNESS_DEBUG") != "" {
		lvl = slog.LevelInfo
	}
	if c.logSink == nil {
		c.logSink = &testWriter{t: c.T}
	}
	h := slog.NewTextHandler(c.logSink, &slog.HandlerOptions{
		Level: lvl,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	return slog.New(h).With("node", id)
}

type testWriter struct {
	t *testing.T

	mu     sync.Mutex
	closed bool
	buf    strings.Builder // every line, for LogHas
}

// LogHas reports whether any node has logged a line containing substr.
func (c *Cluster) LogHas(substr string) bool {
	if c.logSink == nil {
		return false
	}
	c.logSink.mu.Lock()
	defer c.logSink.mu.Unlock()
	return strings.Contains(c.logSink.buf.String(), substr)
}

func (w *testWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	closed := w.closed
	w.buf.Write(p)
	w.mu.Unlock()
	if closed {
		// The node goroutines outlive the test (a final tick can race
		// Stop's cleanup); logging into a finished test panics.
		return len(p), nil
	}
	w.t.Logf("%s", p)
	return len(p), nil
}

// start launches a node's reconcile loop.
func (c *Cluster) start(n *Node) {
	ctx, cancel := context.WithCancel(c.ctx())
	n.mu.Lock()
	defer n.mu.Unlock()
	n.alive = true
	n.cancelRun = cancel
	done := make(chan struct{})
	rt := n.RT
	go func() {
		defer close(done)
		rt.Run(ctx)
	}()
	n.done = done
}

// CreateVolume writes the spec + initial status the leader-side
// placement would (placeVolume's record shape), then lets the
// runtimes converge.
func (c *Cluster) CreateVolume(volID string, size uint64, placement []string) {
	c.T.Helper()
	spec := storage.Spec{
		ID:          volID,
		Name:        volID,
		Namespace:   "default",
		SizeBytes:   size,
		Class:       storage.DefaultStorageClass().Name,
		Replication: len(placement),
	}
	st := storage.Status{State: storage.StateHealthy, Primary: placement[0]}
	for i, nid := range placement {
		role := storage.RoleSecondary
		if i == 0 {
			role = storage.RolePrimary
		}
		st.Placement = append(st.Placement, storage.Replica{
			NodeID:   nid,
			Role:     role,
			ZvolPath: fmt.Sprintf("%s/volumes/%s", pool, volID),
			Healthy:  true,
		})
	}
	if err := storage.SaveSpec(c.ctx(), c.St, spec); err != nil {
		c.T.Fatal(err)
	}
	if err := storage.SaveStatus(c.ctx(), c.St, volID, st); err != nil {
		c.T.Fatal(err)
	}
}

// Elect CASes the status primary to nodeID (the controller's election
// write), preserving placement.
func (c *Cluster) Elect(volID, nodeID string) {
	c.T.Helper()
	// Retried: the runtime writes its own status updates (e.g.
	// reportSequence) concurrently, same as a real controller racing
	// real agents — a real controller retries its own CAS on conflict
	// rather than giving up, so this fake stand-in for one should too.
	for attempt := 0; attempt < 10; attempt++ {
		st, rev, err := storage.LoadStatus(c.ctx(), c.St, volID)
		if err != nil {
			c.T.Fatal(err)
		}
		st.Primary = nodeID
		for i := range st.Placement {
			switch {
			case st.Placement[i].NodeID == nodeID:
				st.Placement[i].Role = storage.RolePrimary
			case st.Placement[i].Role != storage.RoleStale: // the real controller never clears Stale
				st.Placement[i].Role = storage.RoleSecondary
			}
		}
		if err := storage.CompareAndSwapStatus(c.ctx(), c.St, volID, rev, st); err != nil {
			if attempt < 9 {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			c.T.Fatalf("elect %s: %v", nodeID, err)
		}
		return
	}
}

// Status loads the volume status.
func (c *Cluster) Status(volID string) storage.Status {
	c.T.Helper()
	st, _, err := storage.LoadStatus(c.ctx(), c.St, volID)
	if err != nil {
		c.T.Fatal(err)
	}
	return st
}

// Write drives one replicated data write through the volume's primary
// coordinator (the real quorum path).
func (c *Cluster) Write(volID string, off int64, data []byte) error {
	prim := c.Status(volID).Primary
	n := c.byID[prim]
	if n == nil || !n.Alive() {
		return fmt.Errorf("primary %q is not a live node", prim)
	}
	return n.Runtime().WriteOp(volID, off, data)
}

// Flush drives a replicated flush marker (the durability barrier).
func (c *Cluster) Flush(volID string) error {
	prim := c.Status(volID).Primary
	n := c.byID[prim]
	if n == nil || !n.Alive() {
		return fmt.Errorf("primary %q is not a live node", prim)
	}
	return n.Runtime().FlushOp(volID)
}

// Crash stops a node: the runtime's stop path uses Held.Abandon, so
// the volume lease record expires naturally (≤ TTL) — a faithful
// daemon-death analog. Zvol files and the durable oplog survive.
func (c *Cluster) Crash(id string) {
	n := c.byID[id]
	if n == nil {
		c.T.Fatalf("no node %q", id)
	}
	c.stopNode(n)
}

// WipePool destroys a node's fake pool (zvol + durable oplog — one
// crash domain): the post-crash "zvol lost" case the VM runs hit.
func (c *Cluster) WipePool(id string) {
	n := c.byID[id]
	if n == nil {
		c.T.Fatalf("no node %q", id)
	}
	if err := os.RemoveAll(filepath.Join(n.Root, "pool")); err != nil {
		c.T.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(n.Root, "dev")); err != nil {
		c.T.Fatal(err)
	}
}

// Restart brings a crashed node back up over its surviving disk state
// (same root: oplog reload, zvol reuse), with a fresh port.
func (c *Cluster) Restart(id string) {
	c.T.Helper()
	n := c.byID[id]
	if n == nil {
		c.T.Fatalf("no node %q", id)
	}
	if n.Alive() {
		return
	}
	nn := c.newNode(id, n.Root, n.Idx)
	n.mu.Lock()
	n.RT = nn.RT
	n.Port = nn.Port
	n.mu.Unlock()
	c.start(n)
}

// waitFor polls cond until it passes or the deadline expires.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}
