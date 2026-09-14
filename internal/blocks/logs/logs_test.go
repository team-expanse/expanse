package logs

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"

	"google.golang.org/grpc"

	pb "github.com/expanse/expanse/proto"
)

// ---- Query construction (pure; no journald) ----

func TestQueryArgs(t *testing.T) {
	q := Query{Namespace: "default", Name: "web", Replica: 2}
	got := strings.Join(q.Args(Identifier("default", "web", 2)), " ")
	want := "journalctl -q -o json --syslog-identifier=expanse-block-default-web-2"
	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestQueryArgsFollowTailSince(t *testing.T) {
	q := Query{Namespace: "prod", Name: "db", Replica: 0, Follow: true, Tail: 100, Since: "1h"}
	got := strings.Join(q.Args(Identifier("prod", "db", 0)), " ")
	want := "journalctl -q -o json --syslog-identifier=expanse-block-prod-db-0 -n 100 --since 1h -f"
	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestIdentifier(t *testing.T) {
	if got := Identifier("default", "web", 3); got != "expanse-block-default-web-3" {
		t.Errorf("identifier = %q", got)
	}
}

func TestQueryValidate(t *testing.T) {
	ok := Query{Namespace: "d", Name: "n"}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid query rejected: %v", err)
	}
	for name, q := range map[string]Query{
		"no-namespace": {Name: "n"},
		"no-name":      {Namespace: "d"},
		"neg-tail":     {Namespace: "d", Name: "n", Tail: -1},
		"bad-since":    {Namespace: "d", Name: "n", Since: "tomorrow"},
		"neg-since":    {Namespace: "d", Name: "n", Since: "-5m"},
	} {
		if err := q.Validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if err := (Query{Namespace: "d", Name: "n", Since: "2024-01-01T00:00:00Z"}).Validate(); err != nil {
		t.Errorf("RFC3339 since rejected: %v", err)
	}
}

// ---- Reader: parse + pass-through via injected exec ----

func fakeExec(output string) execFunc {
	return func(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
		return fakeCmd(ctx, output)
	}
}

func fakeCmd(ctx context.Context, output string) (*exec.Cmd, error) {
	// Single-quote the output; test payloads never contain single quotes.
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "printf '%s' '"+output+"'")
	return cmd, nil
}

func TestReaderParsesJournalJSON(t *testing.T) {
	out := `{"__REALTIME_TIMESTAMP":"1718000000123456789","MESSAGE":"hello"}
{"__REALTIME_TIMESTAMP":"1718000000223456789","MESSAGE":"world"}
not-json-line`
	r := JournaldReader{Exec: fakeExec(out)}
	lines, err := r.Stream(context.Background(), Query{Namespace: "d", Name: "n"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var got []Line
	for l := range lines {
		got = append(got, l)
	}
	if len(got) != 3 {
		t.Fatalf("got %d lines, want 3: %+v", len(got), got)
	}
	if got[0].TimestampNs != 1718000000123456789 || got[0].Text != "hello" {
		t.Errorf("line0 = %+v", got[0])
	}
	if got[2].Text != "not-json-line" || got[2].TimestampNs != 0 {
		t.Errorf("passthrough line = %+v", got[2])
	}
}

func TestReaderExecFailure(t *testing.T) {
	r := JournaldReader{Exec: func(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
		return nil, errors.New("journalctl: not found")
	}}
	lines, err := r.Stream(context.Background(), Query{Namespace: "d", Name: "n"})
	if err != nil {
		t.Fatalf("Stream should not fail synchronously: %v", err)
	}
	l := <-lines
	if !strings.HasPrefix(l.Text, "log stream error:") {
		t.Errorf("error line = %+v", l)
	}
}

// ---- Proxy routing ----

type fakeStreamClient struct {
	pb.BlockServiceClient
	lines []*pb.LogLine
}

func (f *fakeStreamClient) StreamLogs(ctx context.Context, req *pb.LogsRequest, opts ...grpc.CallOption) (pb.BlockService_StreamLogsClient, error) {
	return &fakeStream{lines: f.lines}, nil
}

type fakeStream struct {
	pb.BlockService_StreamLogsClient
	lines []*pb.LogLine
	i     int
}

func (s *fakeStream) Recv() (*pb.LogLine, error) {
	if s.i >= len(s.lines) {
		return nil, io.EOF
	}
	l := s.lines[s.i]
	s.i++
	return l, nil
}

func TestProxyRoute(t *testing.T) {
	pls := []*pb.PlacementStatus{
		{ReplicaIndex: 0, NodeId: "node-a"},
		{ReplicaIndex: 1, NodeId: "node-b"},
		{ReplicaIndex: 2, NodeId: "node-a"},
	}
	p := &Proxy{Placements: func(ctx context.Context, ns, name string) ([]*pb.PlacementStatus, error) {
		return pls, nil
	}}
	ctx := context.Background()

	// Replica 1 lives on node-b.
	nodes, err := p.Route(ctx, &pb.LogsRequest{Namespace: "d", Name: "n", Replica: 1})
	if err != nil || len(nodes) != 1 || nodes[0] != "node-b" {
		t.Errorf("route replica1 = %v, %v; want [node-b]", nodes, err)
	}
	// Replica 2 lives on node-a (not just index lookup).
	nodes, _ = p.Route(ctx, &pb.LogsRequest{Namespace: "d", Name: "n", Replica: 2})
	if len(nodes) != 1 || nodes[0] != "node-a" {
		t.Errorf("route replica2 = %v; want [node-a]", nodes)
	}
	// All replicas: hosting nodes in replica-index order.
	nodes, err = p.Route(ctx, &pb.LogsRequest{Namespace: "d", Name: "n", Replica: -1})
	if err != nil || len(nodes) != 3 || nodes[0] != "node-a" || nodes[1] != "node-b" || nodes[2] != "node-a" {
		t.Errorf("route all = %v, %v", nodes, err)
	}
	// Missing replica placement → not found.
	if _, err := p.Route(ctx, &pb.LogsRequest{Namespace: "d", Name: "n", Replica: 7}); err == nil {
		t.Error("replica 7 routed without error")
	}
	// Store failure propagates.
	pfail := &Proxy{Placements: func(ctx context.Context, ns, name string) ([]*pb.PlacementStatus, error) {
		return nil, errors.New("store down")
	}}
	if _, err := pfail.Route(ctx, &pb.LogsRequest{Namespace: "d", Name: "n"}); err == nil {
		t.Error("placements error swallowed")
	}
}

// ---- CLI flag parsing ----

func TestParseFlags(t *testing.T) {
	type tc struct {
		name    string
		replica int32
		all     bool
		follow  bool
		tail    int
		since   string
		wantErr bool
		check   func(Query) bool
	}
	for _, c := range []tc{
		{name: "defaults", check: func(q Query) bool { return !q.Follow && q.Tail == 0 && !q.AllReplicas && q.Replica == 0 }},
		{
			name: "follow-tail-since", replica: 1, follow: true, tail: 50, since: "1h",
			check: func(q Query) bool { return q.Follow && q.Tail == 50 && q.Replica == 1 && q.Since == "1h" },
		},
		{name: "all-replicas", all: true, check: func(q Query) bool { return q.AllReplicas && q.Replica == -1 }},
		{name: "bad-since", since: "yesterday", wantErr: true},
		{name: "neg-tail", tail: -3, wantErr: true},
	} {
		q, err := ParseFlags(Flags{Replica: c.replica, AllReplicas: c.all, Follow: c.follow, Tail: c.tail, Since: c.since})
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: want error", c.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !c.check(q) {
			t.Errorf("%s: query = %+v", c.name, q)
		}
	}
}

// ---- StreamLogs end-to-end over the proxy with a fake dial ----

func TestProxyStreamLogs(t *testing.T) {
	pls := []*pb.PlacementStatus{{ReplicaIndex: 0, NodeId: "node-a"}}
	p := &Proxy{
		Placements: func(ctx context.Context, ns, name string) ([]*pb.PlacementStatus, error) { return pls, nil },
		Dial: func(ctx context.Context, nodeID string) (pb.BlockServiceClient, error) {
			return &fakeStreamClient{lines: []*pb.LogLine{
				{ReplicaIndex: 0, Line: "line-one"},
				{ReplicaIndex: 0, Line: "line-two"},
			}}, nil
		},
	}
	var got []*pb.LogLine
	err := p.StreamLogs(context.Background(), &pb.LogsRequest{Namespace: "d", Name: "n"}, func(l *pb.LogLine) error {
		got = append(got, l)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	if len(got) != 2 || got[0].Line != "line-one" {
		t.Errorf("lines = %+v", got)
	}
}
