// Package logs implements block log streaming (PHASE04.md §5.5): reading
// block logs from journald (SYSLOG_IDENTIFIER=expanse-block-<ns>-<name>-<idx>),
// the agent-side StreamLogs gRPC handler, and the API-server proxy that
// forwards to the hosting node's agent (modeled on raftstore's forwarding
// shape: look up where the replica lives, dial, stream).
//
// Journal access deviation (documented per T01): the sdjournal cgo binding
// is excluded by the repo's CGO_ENABLED=0 build, so the reader shells out
// to journalctl(1) — no build tag required, works under any cross-build.
// Query construction is a pure function (Args), unit-tested without
// touching a real journald.
package logs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/errors"
	pb "github.com/expanse/expanse/proto"
)

// Identifier is the journald SYSLOG_IDENTIFIER for one replica (§5.5),
// matching the systemd unit's SyslogIdentifier.
func Identifier(namespace, name string, replica int32) string {
	return fmt.Sprintf("expanse-block-%s-%s-%d", namespace, name, replica)
}

// Line is one streamed log line.
type Line struct {
	ReplicaIndex int32
	TimestampNs  int64
	Text         string
}

// Query describes a log request (the CLI/gRPC surface, §5.5/§7).
type Query struct {
	Namespace   string
	Name        string
	Replica     int32  // single replica when !AllReplicas
	AllReplicas bool   // --all-replicas
	Follow      bool   // -f
	Tail        int    // --tail N (0 = journald default)
	Since       string // --since: duration ("1h") or RFC3339
}

// Validate checks flag combinations (V-rules for logs).
func (q Query) Validate() error {
	if q.Namespace == "" || q.Name == "" {
		return errors.New(errors.KindInvalid, "logs.Query", "namespace and name are required")
	}
	if q.Tail < 0 {
		return errors.New(errors.KindInvalid, "logs.Query", "tail must be >= 0")
	}
	if q.Since != "" {
		if _, err := parseSince(q.Since); err != nil {
			return err
		}
	}
	return nil
}

// parseSince accepts a duration ("1h", "30m") or an RFC3339 timestamp.
func parseSince(s string) (string, error) {
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return "", errors.New(errors.KindInvalid, "logs.since", "duration must be positive")
		}
		return s, nil
	}
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		return "", errors.New(errors.KindInvalid, "logs.since", "must be a duration (1h) or RFC3339 timestamp")
	}
	return s, nil
}

// Identifiers returns the journal identifiers to read: one, or all
// replicas 0..maxReplica for --all-replicas.
func (q Query) Identifiers(maxReplica int32) []string {
	if !q.AllReplicas {
		return []string{Identifier(q.Namespace, q.Name, q.Replica)}
	}
	out := make([]string, 0, maxReplica+1)
	for i := int32(0); i <= maxReplica; i++ {
		out = append(out, Identifier(q.Namespace, q.Name, i))
	}
	return out
}

// Args renders the journalctl invocation for one identifier (pure — the
// unit-tested surface; no journald needed to verify query construction).
func (q Query) Args(identifier string) []string {
	args := []string{"journalctl", "-q", "-o", "json", "--identifier=" + identifier}
	if q.Tail > 0 && !q.Follow {
		args = append(args, "-n", strconv.Itoa(q.Tail))
	}
	if q.Tail > 0 && q.Follow {
		// follow starts from the tail window, then streams
		args = append(args, "-n", strconv.Itoa(q.Tail))
	}
	if q.Since != "" {
		args = append(args, "--since", q.Since)
	}
	if q.Follow {
		args = append(args, "-f")
	}
	return args
}

// Reader streams log lines for a query.
type Reader interface {
	Stream(ctx context.Context, q Query) (<-chan Line, error)
}

// journalLine is the journalctl -o json envelope we consume.
type journalLine struct {
	RealtimeTimestamp string `json:"__REALTIME_TIMESTAMP"`
	Message           string `json:"MESSAGE"`
}

// JournaldReader shells out to journalctl(1); see the package doc for the
// sdjournal deviation. Exec is injectable for tests.
type JournaldReader struct {
	// Exec runs the command; nil means real execution.
	Exec func(ctx context.Context, name string, args ...string) (*exec.Cmd, error)
}

// Stream implements Reader. The returned channel closes at EOF (or on
// follow, when ctx is cancelled). Errors surface on the first channel
// send as a zero Line with Text starting "log stream error:".
func (r JournaldReader) Stream(ctx context.Context, q Query) (<-chan Line, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	run := realExec(r.Exec)
	out := make(chan Line)
	go func() {
		defer close(out)
		for _, id := range q.Identifiers(0) {
			// AllReplicas interleaving: sequential per replica in index
			// order (each line carries its replica index; callers prefix
			// accordingly). Identifiers(0) yields the single-replica case;
			// Proxy drives the multi-replica loop instead.
			r.streamOne(ctx, q, id, run, out)
		}
	}()
	return out, nil
}

func (r JournaldReader) streamOne(ctx context.Context, q Query, identifier string, run execFunc, out chan<- Line) {
	args := q.Args(identifier)
	cmd, err := run(ctx, args[0], args[1:]...)
	if err != nil {
		sendError(out, 0, err)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		sendError(out, 0, err)
		return
	}
	if err := cmd.Start(); err != nil {
		sendError(out, 0, err)
		return
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var jl journalLine
		if err := json.Unmarshal(sc.Bytes(), &jl); err != nil {
			// Non-JSON line: pass through verbatim.
			out <- Line{Text: strings.TrimRight(sc.Text(), "\n")}
			continue
		}
		ts, _ := strconv.ParseInt(jl.RealtimeTimestamp, 10, 64)
		out <- Line{TimestampNs: ts, Text: jl.Message}
	}
	_ = cmd.Wait() // exit status irrelevant; context cancel kills -f
}

type execFunc func(ctx context.Context, name string, args ...string) (*exec.Cmd, error)

func defaultExec(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, name, args...), nil
}

// realExec adapts the injected Exec func to execFunc.
func realExec(f func(ctx context.Context, name string, args ...string) (*exec.Cmd, error)) execFunc {
	if f == nil {
		return defaultExec
	}
	return execFunc(f)
}

func sendError(out chan<- Line, replica int32, err error) {
	out <- Line{ReplicaIndex: replica, Text: "log stream error: " + err.Error()}
}

// Flags is the raw CLI flag surface for `expanse ctl block logs`.
type Flags struct {
	Replica     int32
	AllReplicas bool
	Follow      bool
	Tail        int
	Since       string
}

// ParseFlags turns raw CLI flags into a Query. --all-replicas selects
// every replica (Replica becomes -1, the "all" sentinel the proxy
// expands back to per-replica routes).
func ParseFlags(f Flags) (Query, error) {
	q := Query{
		Namespace:   "", // set by the caller from the positional arg
		Name:        "",
		Replica:     f.Replica,
		AllReplicas: f.AllReplicas,
		Follow:      f.Follow,
		Tail:        f.Tail,
		Since:       f.Since,
	}
	if f.AllReplicas {
		q.Replica = -1
	}
	// Namespace/name are set from the positional arg by the caller, so
	// only flag-level validation happens here.
	if err := (Query{Namespace: "x", Name: "y", Tail: q.Tail, Since: q.Since}).Validate(); err != nil {
		return Query{}, err
	}
	return q, nil
}

// ---- gRPC surfaces ----

// AgentServer is the agent-side StreamLogs handler: it reads the local
// journal for the requested replica and streams lines out. The agent
// registers it on its BlockService; only lines for replicas hosted here
// produce output (journald filtering by identifier handles that).
type AgentServer struct {
	Reader Reader
}

// StreamLogs implements the agent side of BlockService.StreamLogs.
func (s AgentServer) StreamLogs(req *pb.LogsRequest, srv pb.BlockService_StreamLogsServer) error {
	q := Query{
		Namespace: req.GetNamespace(),
		Name:      req.GetName(),
		Replica:   req.GetReplica(),
		Follow:    req.GetFollow(),
		Tail:      int(req.GetTail()),
		Since:     req.GetSince(),
	}
	lines, err := s.Reader.Stream(srv.Context(), q)
	if err != nil {
		return err
	}
	for l := range lines {
		if err := srv.Send(&pb.LogLine{
			ReplicaIndex:    l.ReplicaIndex,
			TimestampUnixNs: l.TimestampNs,
			Line:            l.Text,
		}); err != nil {
			return err
		}
	}
	return nil
}

// Proxy is the API-server-side forwarder (§5.5: "The API server on any
// node proxies to the hosting node's agent"). Modeled on raftstore's
// forwarding shape: look up where the replica lives, dial, stream. Both
// hooks are injectable; the production wiring reads placements from the
// replicated store and dials the node's gRPC endpoint.
type Proxy struct {
	// Placements returns the block's replica placements, indexed by
	// replica index (holes nil for missing replicas).
	Placements func(ctx context.Context, ns, name string) ([]*pb.PlacementStatus, error)
	// Dial opens a BlockService client to the node hosting the replica.
	Dial func(ctx context.Context, nodeID string) (pb.BlockServiceClient, error)
}

// Route returns the node hosting req's replica (or, for --all-replicas /
// replica<0, all hosting nodes in replica-index order).
func (p *Proxy) Route(ctx context.Context, req *pb.LogsRequest) ([]string, error) {
	pls, err := p.Placements(ctx, req.GetNamespace(), req.GetName())
	if err != nil {
		return nil, err
	}
	if req.GetReplica() < 0 {
		var nodes []string
		for _, pl := range pls {
			if pl != nil && pl.GetNodeId() != "" {
				nodes = append(nodes, pl.GetNodeId())
			}
		}
		if len(nodes) == 0 {
			return nil, errors.New(errors.KindNotFound, "logs.Route", "block has no placements")
		}
		return nodes, nil
	}
	if int(req.GetReplica()) >= len(pls) || pls[req.GetReplica()] == nil || pls[req.GetReplica()].GetNodeId() == "" {
		return nil, errors.New(errors.KindNotFound, "logs.Route",
			fmt.Sprintf("replica %d has no placement", req.GetReplica()))
	}
	return []string{pls[req.GetReplica()].GetNodeId()}, nil
}

// StreamLogs routes the request and copies lines to send, interleaving
// per-replica streams sequentially in replica-index order with the
// replica index stamped on every line (CLI prefixes per line).
func (p *Proxy) StreamLogs(ctx context.Context, req *pb.LogsRequest, send func(*pb.LogLine) error) error {
	nodes, err := p.Route(ctx, req)
	if err != nil {
		return err
	}
	replica := req.GetReplica()
	for i, node := range nodes {
		if req.GetReplica() < 0 {
			replica = int32(i)
		}
		client, err := p.Dial(ctx, node)
		if err != nil {
			return err
		}
		stream, err := client.StreamLogs(ctx, &pb.LogsRequest{
			Namespace: req.GetNamespace(), Name: req.GetName(),
			Replica: replica,
			Follow:  req.GetFollow(), Tail: req.GetTail(), Since: req.GetSince(),
		})
		if err != nil {
			return err
		}
		for {
			line, err := stream.Recv()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				break // this replica's stream ended; next replica
			}
			if err := send(line); err != nil {
				return err
			}
		}
	}
	return nil
}
