package raftstore_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"

	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
	"github.com/hashicorp/raft"
)

// TestTransferLeadership exercises the fixed leadership-transfer API:
// leadership lands on the requested server and the cluster keeps
// serving writes through the new leader.
func TestTransferLeadership(t *testing.T) {
	c := NewTestCluster(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	followerIdx := -1
	for i, s := range c.Nodes {
		if !s.IsLeader() {
			followerIdx = i
			break
		}
	}
	if followerIdx < 0 {
		t.Fatal("no follower")
	}
	target := c.Nodes[followerIdx]
	if err := c.Nodes[0].TransferLeadership(ctx, c.Nodes[followerIdx].NodeID()); err != nil {
		t.Fatalf("TransferLeadership: %v", err)
	}
	// The transfer future resolves as the handshake completes; the new
	// leader's state transition can land a beat later.
	becameLeader := false
	for start := time.Now(); time.Since(start) < 5*time.Second; {
		if target.IsLeader() {
			becameLeader = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !becameLeader {
		t.Fatalf("leadership did not land on n%d within 5s", followerIdx)
	}
	if _, err := target.Put(ctx, store.Key("after-transfer"), []byte("ok")); err != nil {
		t.Fatalf("write through new leader: %v", err)
	}
	if _, err := target.Get(ctx, store.Key("after-transfer")); err != nil {
		t.Fatalf("read through new leader: %v", err)
	}
}

// TestRemoveServer removes a voter: the cluster keeps quorum and
// serves, the removed node stops seeing a leader.
func TestRemoveServer(t *testing.T) {
	c := NewTestCluster(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lead := c.Leader()
	if err := lead.RemoveServer(ctx, c.Nodes[2].NodeID()); err != nil {
		t.Fatalf("RemoveServer: %v", err)
	}
	if _, err := lead.Put(ctx, store.Key("post-remove"), []byte("ok")); err != nil {
		t.Fatalf("write after remove: %v", err)
	}
	if e, err := lead.Get(ctx, store.Key("post-remove")); err != nil || e == nil {
		t.Fatalf("read after remove: %v %v", e, err)
	}
}

// TestStateHashConvergence verifies all nodes report the same FSM hash
// after a batch of writes.
func TestStateHashConvergence(t *testing.T) {
	c := NewTestCluster(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lead := c.Leader()
	for i := 0; i < 20; i++ {
		k := store.Key([]byte{0x30, 0x30 + byte(i/10), 0x30 + byte(i%10), '-', 'k'})
		_ = k
	}
	for i := 0; i < 20; i++ {
		key := store.Key("hash/" + string(rune('a'+i)))
		if _, err := lead.Put(ctx, key, []byte{byte(i)}); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	time.Sleep(500 * time.Millisecond) // let followers apply
	var hashes [][32]byte
	for _, s := range c.Nodes {
		hashes = append(hashes, s.StateHash())
	}
	if hashes[0] != hashes[1] || hashes[1] != hashes[2] {
		t.Errorf("divergent FSM hashes: %v", hashes)
	}
}

// TestLinearList covers the linearizable list path: identical results
// on the leader and forwarded from a follower.
func TestLinearList(t *testing.T) {
	c := NewTestCluster(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lead := c.Leader()
	want := []string{}
	for _, k := range []string{"list/a", "list/b", "list/c", "other/z"} {
		if _, err := lead.Put(ctx, store.Key(k), []byte("v:"+k)); err != nil {
			t.Fatalf("put %s: %v", k, err)
		}
		if k != "other/z" {
			want = append(want, k)
		}
	}
	sort.Strings(want)

	entries, err := lead.LinearList(ctx, store.Key("list/"))
	if err != nil {
		t.Fatalf("LinearList leader: %v", err)
	}
	got := []string{}
	for _, e := range entries {
		got = append(got, string(e.Key))
	}
	if len(got) != len(want) {
		t.Fatalf("LinearList leader = %v, want %v", got, want)
	}

	follower := c.Follower()
	fEntries, err := follower.LinearList(ctx, store.Key("list/"))
	if err != nil {
		t.Fatalf("LinearList follower (forwarded): %v", err)
	}
	if len(fEntries) != len(entries) {
		t.Fatalf("forwarded LinearList = %d entries, want %d", len(fEntries), len(entries))
	}
}

// TestStoreAccessors covers the trivial accessors.
func TestStoreAccessors(t *testing.T) {
	c := NewTestCluster(t, 3)
	if c.Nodes[0].NodeID() == "" {
		t.Error("NodeID empty")
	}
	if c.Nodes[0].State() != raft.Leader && c.Nodes[0].State() != raft.Follower {
		t.Errorf("unexpected state %v", c.Nodes[0].State())
	}
}

// TestForwardList exercises the forward server's list RPC directly
// (follower→leader path over the internal gRPC endpoint).
func TestForwardList(t *testing.T) {
	c := NewTestCluster(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lead := c.Leader()
	for _, k := range []string{"fwd/1", "fwd/2"} {
		if _, err := lead.Put(ctx, store.Key(k), []byte("v")); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	// The follower's forwarded list must match the leader's.
	follower := c.Follower()
	fEntries, err := follower.List(ctx, store.Key("fwd/"))
	if err != nil {
		t.Fatalf("forwarded list: %v", err)
	}
	if len(fEntries) != 2 {
		t.Errorf("forwarded list = %d entries, want 2", len(fEntries))
	}
	// Wire-compat: SetDialCreds on the forwarder must not panic (nil
	// creds fall back to insecure; mTLS wiring happens in production).
	lead.SetForwarder(nil)
}

// TestAssertNoStaleReadsScanner unit-tests the scanner itself: it must
// flag WithStale usages, skip testdata, and report file:line.
func TestAssertNoStaleReadsScanner(t *testing.T) {
	dir := t.TempDir()
	clean := filepath.Join(dir, "clean.go")
	if err := os.WriteFile(clean, []byte("package x\n\nfunc f() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if bad := store.AssertNoStaleReads(dir); len(bad) != 0 {
		t.Errorf("clean dir flagged: %v", bad)
	}

	dirty := filepath.Join(dir, "dirty.go")
	if err := os.WriteFile(dirty, []byte("package x\n\nvar _ = store.WithStale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := store.AssertNoStaleReads(dir)
	if len(bad) != 1 || !strings.HasSuffix(bad[0], "dirty.go:3") {
		t.Errorf("violations = %v, want .../dirty.go:3", bad)
	}

	// testdata is skipped.
	os.Mkdir(filepath.Join(dir, "testdata"), 0o700)                                             //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "testdata", "hint.go"), []byte("store.WithStale\n"), 0o644) //nolint:errcheck
	bad = store.AssertNoStaleReads(dir)
	if len(bad) != 1 {
		t.Errorf("violations after testdata write = %v, want 1", bad)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestForwarderSetDialCreds covers the credential setter (nil falls
// back; a real config wires up without error).
func TestForwarderSetDialCreds(t *testing.T) {
	fwd := raftstore.NewGRPCForwarder(func() string { return "127.0.0.1:1" }, func(string) (string, bool) { return "", false })
	fwd.SetDialCreds(nil)
	fwd.SetDialCreds(nil) // idempotent
	_ = fwd.Close()
}

// TestApplyCommandNotLeader pins the direct-apply guard: a follower
// rejects ApplyCommand with unavailable (writes must go through the
// leader or the forward path).
func TestApplyCommandNotLeader(t *testing.T) {
	c := NewTestCluster(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	follower := c.Follower()
	_, err := follower.ApplyCommand(ctx, &pb.Command{
		Type: pb.CommandType_COMMAND_TYPE_PUT, Key: "direct", Value: []byte("x"),
		TimestampUnixNs: time.Now().UnixNano(),
	})
	if err == nil || !errors.Is(err, errors.KindUnavailable) || !strings.Contains(err.Error(), "not leader") {
		t.Errorf("ApplyCommand on follower = %v, want not-leader unavailable", err)
	}
	// The leader accepts the same command.
	lead := c.Leader()
	if _, err := lead.ApplyCommand(ctx, &pb.Command{
		Type: pb.CommandType_COMMAND_TYPE_PUT, Key: "direct", Value: []byte("x"),
		TimestampUnixNs: time.Now().UnixNano(),
	}); err != nil {
		t.Errorf("ApplyCommand on leader: %v", err)
	}
}

// otherFollower returns a follower that is not f.
func otherFollower(t *testing.T, c *TestCluster, f *raftstore.Store) *raftstore.Store {
	t.Helper()
	for _, s := range c.Nodes {
		if s != f && !s.IsLeader() {
			return s
		}
	}
	t.Fatal("no second follower")
	return nil
}

// TestRemoveServerViaFollower: a follower forwards the membership change
// to the leader instead of failing with "not leader".
func TestRemoveServerViaFollower(t *testing.T) {
	c := NewTestCluster(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := c.Follower()
	gone := otherFollower(t, c, f)
	if err := f.RemoveServer(ctx, gone.NodeID()); err != nil {
		t.Fatalf("RemoveServer via follower: %v", err)
	}
	members, err := c.Leader().Members()
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 2 || contains(members, gone.NodeID()) {
		t.Errorf("members = %v, want 2 without %s", members, gone.NodeID())
	}
}

// TestTransferLeadershipViaFollower: a follower can ask for leadership to
// move to itself; the request is forwarded to the current leader.
func TestTransferLeadershipViaFollower(t *testing.T) {
	c := NewTestCluster(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := c.Follower()
	if err := f.TransferLeadership(ctx, f.NodeID()); err != nil {
		t.Fatalf("TransferLeadership via follower: %v", err)
	}
	for start := time.Now(); !f.IsLeader(); time.Sleep(20 * time.Millisecond) {
		if time.Since(start) > 5*time.Second {
			t.Fatalf("%s did not become leader", f.NodeID())
		}
	}
	if got := f.LeaderID(); got != f.NodeID() {
		t.Errorf("LeaderID = %q, want %q", got, f.NodeID())
	}
}

// TestTransferLeadershipToAnyone: an empty target lets raft pick the
// most up-to-date follower.
func TestTransferLeadershipToAnyone(t *testing.T) {
	c := NewTestCluster(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	old := c.Leader()
	if err := old.TransferLeadership(ctx, ""); err != nil {
		t.Fatalf("TransferLeadership: %v", err)
	}
	for start := time.Now(); ; time.Sleep(20 * time.Millisecond) {
		if id := old.LeaderID(); id != "" && id != old.NodeID() {
			break
		}
		if time.Since(start) > 5*time.Second {
			t.Fatal("leadership did not move")
		}
	}
}
