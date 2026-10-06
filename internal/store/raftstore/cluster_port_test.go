package raftstore_test

import (
	"fmt"
	"net"
	"testing"
)

// TestClusterStartsWhenRaftPortPlus1000IsTaken guards against deriving api ports from raft ports.
func TestClusterStartsWhenRaftPortPlus1000IsTaken(t *testing.T) {
	orig := freePort
	t.Cleanup(func() { freePort = orig })
	freePort = func(t *testing.T) int {
		t.Helper()
		for {
			p := orig(t)
			if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p+1000)); err == nil {
				t.Cleanup(func() { ln.Close() })
				return p
			}
		}
	}
	c := NewTestCluster(t, 3)
	c.Kill(1)
	c.Restart(1)
	if c.Leader() == nil {
		t.Fatal("no leader after restart")
	}
}
