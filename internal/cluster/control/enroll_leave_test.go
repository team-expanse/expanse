package control_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// TestLeaveGuardrails covers the safety interlocks: never remove the
// leader, never shrink below two nodes.
func TestLeaveGuardrails(t *testing.T) {
	r := newRig(t, "leave-guards")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Leader cannot remove itself.
	err := control.Leave(ctx, r.initRes.Store, "n1")
	if err == nil {
		t.Fatal("Leave(leader) accepted")
	}
	if !strings.Contains(err.Error(), "leader") {
		t.Errorf("Leave(leader) error = %v, want leader-guard", err)
	}

	// Cannot shrink below two nodes.
	err = control.Leave(ctx, r.initRes.Store, "n2")
	if err == nil {
		t.Fatal("Leave shrinking below 2 accepted")
	}
	if !strings.Contains(err.Error(), "2 nodes") {
		t.Errorf("Leave quorum-guard error = %v", err)
	}
}

// TestLeaveRemovesVoter runs a real leave on a three-node cluster:
// RemoveServer + node-record delete, quorum preserved.
func TestLeaveRemovesVoter(t *testing.T) {
	r := newRig(t, "leave-voter")
	r.enrollNode("n2", "voter")
	r.enrollNode("n3", "voter")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := control.Leave(ctx, r.initRes.Store, "n3"); err != nil {
		t.Fatalf("Leave(n3): %v", err)
	}
	if _, err := r.initRes.Store.Get(ctx, store.Key(join.NodesKeyPrefix+"n3")); !errors.Is(err, errors.KindNotFound) {
		t.Errorf("node record n3 still present (err %v)", err)
	}
}

// TestEnrollValidation covers the fast-fail argument checks and the
// already-a-cluster-node conflict.
func TestEnrollValidation(t *testing.T) {
	r := newRig(t, "enroll-validate")
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	cases := []struct {
		name string
		opts control.EnrollOptions
		want string
	}{
		{"no address", control.EnrollOptions{Token: "t", DataDir: r.t.TempDir()}, "--address"},
		{"no token", control.EnrollOptions{Address: "127.0.0.1:1", DataDir: r.t.TempDir()}, "--token"},
		{"bad role", control.EnrollOptions{Address: "127.0.0.1:1", Token: "t", DataDir: r.t.TempDir(), Role: "emperor"}, "--role"},
	}
	for _, tc := range cases {
		if _, err := control.Enroll(ctx, tc.opts); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want containing %q", tc.name, err, tc.want)
		}
	}

	// Already enrolled data dir → conflict.
	dir := r.t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, control.ClusterIDFile), []byte("some-cluster\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := control.Enroll(ctx, control.EnrollOptions{
		Address: "127.0.0.1:1", Token: "t", DataDir: dir,
	}); err == nil || !strings.Contains(err.Error(), "already a cluster node") {
		t.Errorf("re-enroll: err = %v, want already-a-cluster-node", err)
	}
}

// TestEnrollBadTokenRejected covers the server-side token rejection
// path (post-raft-open, pre-certificate).
func TestEnrollBadTokenRejected(t *testing.T) {
	r := newRig(t, "enroll-bad-token")
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	raftPort := freePort(t)
	_, err := control.Enroll(ctx, control.EnrollOptions{
		DataDir: r.t.TempDir(), NodeID: "nx", Address: r.joinAddr,
		Token: "bogus-token", Role: "voter", APIAddr: "127.0.0.1:1",
		BindAddr:      fmt.Sprintf("127.0.0.1:%d", raftPort),
		AdvertiseAddr: fmt.Sprintf("127.0.0.1:%d", raftPort),
	})
	if err == nil {
		t.Fatal("Enroll with bogus token accepted")
	}
}

// TestEnrollWitnessRole verifies the witness role flows through to the
// node record (§4.9: witnesses vote but are never placeable).
func TestEnrollWitnessRole(t *testing.T) {
	r := newRig(t, "enroll-witness")
	r.enrollNode("nw", "witness")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rec, err := r.initRes.Store.Get(ctx, store.Key(join.NodesKeyPrefix+"nw"))
	if err != nil || rec == nil {
		t.Fatalf("witness node record: %v %v", rec, err)
	}
	if !strings.Contains(string(rec.Value), "witness") {
		t.Errorf("node record missing witness role: %s", rec.Value)
	}
}

// TestInitAlreadyClusterNode covers the bootstrap guard.
func TestInitAlreadyClusterNode(t *testing.T) {
	r := newRig(t, "init-twice")
	dir := r.t.TempDir()
	_ = dir
	if _, err := control.Init(context.Background(), control.InitOptions{
		DataDir: r.dataDir, NodeID: "nz", Name: "again",
		AdvertiseAddr: "127.0.0.1:0", BindAddr: "127.0.0.1:0", JoinHost: "127.0.0.1",
	}); err == nil || !strings.Contains(err.Error(), "already a cluster node") {
		t.Errorf("re-init err = %v, want conflict", err)
	}
}
