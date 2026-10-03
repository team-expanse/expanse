package main

import (
	"strings"
	"testing"
	"time"

	pb "github.com/expanse/expanse/proto"
)

func TestPrintNodeListShowsCordonAndLifecycle(t *testing.T) {
	seen := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	var b strings.Builder
	err := printNodeList(&b, []*pb.ClusterNode{
		{Id: "n1", Role: "voter", Lifecycle: "healthy", RaftAddr: "10.0.0.1:7444", LastSeenUnixNs: seen.UnixNano()},
		{Id: "n2", Lifecycle: "unreachable", Cordoned: true, RaftAddr: "10.0.0.2:7444", LastSeenUnixNs: seen.UnixNano()},
		{Id: "n3", Lifecycle: "healthy", Cordoned: true, Draining: true, RaftAddr: "10.0.0.3:7444", LastSeenUnixNs: seen.UnixNano()},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "ID") {
		t.Fatalf("got:\n%s", b.String())
	}
	for _, want := range []string{"n1", "voter", "healthy", "false", "2026-10-02T12:00:00Z"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("n1 row %q missing %q", lines[1], want)
		}
	}
	for _, want := range []string{"n2", "(none)", "unreachable", "true"} {
		if !strings.Contains(lines[2], want) {
			t.Errorf("n2 row %q missing %q", lines[2], want)
		}
	}
	if !strings.Contains(lines[3], "draining") {
		t.Errorf("n3 row %q missing draining", lines[3])
	}
}
