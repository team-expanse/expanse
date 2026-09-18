package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pbproto "google.golang.org/protobuf/proto"

	pb "github.com/expanse/expanse/proto"
)

// fakeStore is a minimal NodeServiceServer backed by a map — enough
// surface for the volume CLI (Get/List/Put/Delete KeyValue).
type fakeStore struct {
	pb.UnimplementedNodeServiceServer
	kv map[string][]byte
}

func (f *fakeStore) GetKeyValue(_ context.Context, r *pb.GetKeyValueRequest) (*pb.GetKeyValueResponse, error) {
	v, ok := f.kv[r.GetKey()]
	if !ok {
		return &pb.GetKeyValueResponse{Found: false}, nil
	}
	return &pb.GetKeyValueResponse{Found: true, Value: v}, nil
}

func (f *fakeStore) PutKeyValue(_ context.Context, r *pb.PutKeyValueRequest) (*pb.PutKeyValueResponse, error) {
	f.kv[r.GetKey()] = r.GetValue()
	return &pb.PutKeyValueResponse{}, nil
}

func (f *fakeStore) ListKeyValue(_ context.Context, r *pb.ListKeyValueRequest) (*pb.ListKeyValueResponse, error) {
	res := &pb.ListKeyValueResponse{}
	for k, v := range f.kv {
		if strings.HasPrefix(k, r.GetPrefix()) {
			res.Entries = append(res.Entries, &pb.KeyValueEntry{Key: k, Value: v})
		}
	}
	return res, nil
}

// serveCLI runs a fake agent socket and returns a ctlOpts pointing at
// it plus a shutdown func.
func serveCLI(t *testing.T, store *fakeStore) (*ctlOpts, func()) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "agent.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(grpc.Creds(insecure.NewCredentials()))
	pb.RegisterNodeServiceServer(gs, store)
	go gs.Serve(l) //nolint: errcheck — test server
	return &ctlOpts{output: "table", socket: sock, timeout: 5 * time.Second}, gs.Stop
}

func fixtureVolume(t *testing.T) *volEntry {
	t.Helper()
	return &volEntry{
		id:   "vol-abc",
		spec: &pb.VolumeSpec{Name: "db", SizeBytes: 10 << 30},
		st: &pb.VolumeStatus{
			State:    pb.VolumeState_VOLUME_STATE_HEALTHY,
			Primary:  "n1",
			Sequence: 42,
			Placement: []*pb.Replica{
				{NodeId: "n1", Role: pb.ReplicaRole_REPLICA_ROLE_PRIMARY, Sequence: 42, LastSeenUnixNano: time.Now().UnixNano()},
				{NodeId: "n2", Role: pb.ReplicaRole_REPLICA_ROLE_SECONDARY, Sequence: 40, LastSeenUnixNano: time.Now().UnixNano()},
				{NodeId: "n3", Role: pb.ReplicaRole_REPLICA_ROLE_STALE, Sequence: 12},
			},
		},
	}
}

// TestPrintInspectShowsPerReplicaSeqAndLag is §4.8's called-out
// feature: the inspect view answers "is my data safe?" — per-replica
// sequence numbers and lag must be there.
func TestPrintInspectShowsPerReplicaSeqAndLag(t *testing.T) {
	var buf bytes.Buffer
	if err := printInspect(context.Background(), &buf, nil, fixtureVolume(t)); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"volume db (vol-abc)",
		"state:    Healthy",
		"primary:  n1",
		"sequence: 42",
		"n1",
		"n2",
		"n3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect output missing %q:\n%s", want, out)
		}
	}
	// Lags: primary 0, n2 lags by 2, n3 (stale) by 30.
	if !strings.Contains(out, "0") || !strings.Contains(out, "2") || !strings.Contains(out, "30") {
		t.Errorf("inspect output missing lag columns:\n%s", out)
	}
	// Roles render human-readable.
	if !strings.Contains(out, "primary") || !strings.Contains(out, "secondary") || !strings.Contains(out, "stale") {
		t.Errorf("inspect output missing roles:\n%s", out)
	}
}

// TestVolumeCommandSurface: every §4.8 subcommand exists with its flags.
func TestVolumeCommandSurface(t *testing.T) {
	cmd := newVolumeCmd(&ctlOpts{})
	want := map[string][]string{
		"create":       {"size", "class", "replication"},
		"list":         {},
		"delete":       {},
		"inspect":      {},
		"resize":       {"size"},
		"snapshot":     {"name"},
		"restore":      {"snapshot"},
		"move-primary": {"to"},
		"resync":       {"replica", "full"},
		"verify":       {},
	}
	for name, flags := range want {
		sub, _, err := cmd.Find([]string{name})
		if err != nil || sub.Name() != name {
			t.Fatalf("subcommand %q missing (found %v, err %v)", name, sub, err)
		}
		for _, f := range flags {
			if sub.Flags().Lookup(f) == nil {
				t.Errorf("%s: flag --%s missing", name, f)
			}
		}
	}
	div, _, err := cmd.Find([]string{"diverged"})
	if err != nil || div.Name() != "diverged" {
		t.Fatal("subcommand diverged missing")
	}
	if div.Flags().Lookup("choose") == nil {
		t.Error("diverged: flag --choose missing (§9 valve)")
	}
}

// TestInspectAgainstFixture runs the real inspect command against a
// fake agent socket holding fixture state.
func TestInspectAgainstFixture(t *testing.T) {
	v := fixtureVolume(t)
	fs := &fakeStore{kv: map[string][]byte{
		"/volumes/vol-abc/spec":   mustProto(t, v.spec),
		"/volumes/vol-abc/status": mustProto(t, v.st),
	}}
	opts, stop := serveCLI(t, fs)
	defer stop()

	var buf bytes.Buffer
	cmd := newVolumeCmd(opts)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"inspect", "db"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "sequence: 42") || !strings.Contains(buf.String(), "n2") {
		t.Errorf("inspect via socket wrong:\n%s", buf.String())
	}
}

// TestDeleteOpWritesRequest: delete resolves the volume by name and
// writes the op record the controller consumes.
func TestDeleteOpWritesRequest(t *testing.T) {
	v := fixtureVolume(t)
	fs := &fakeStore{kv: map[string][]byte{
		"/volumes/vol-abc/spec":   mustProto(t, v.spec),
		"/volumes/vol-abc/status": mustProto(t, v.st),
	}}
	opts, stop := serveCLI(t, fs)
	defer stop()

	cmd := newVolumeCmd(opts)
	cmd.SetArgs([]string{"delete", "db"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	op, ok := fs.kv["/volumes/_ops/delete/vol-abc"]
	if !ok {
		t.Fatalf("delete op not written; keys: %v", fs.kv)
	}
	if !strings.Contains(string(op), `"target":"db"`) {
		t.Errorf("op payload wrong: %s", op)
	}
}

// TestResyncFlagsParse: --replica is required, --full is honored.
func TestResyncFlagsParse(t *testing.T) {
	v := fixtureVolume(t)
	fs := &fakeStore{kv: map[string][]byte{
		"/volumes/vol-abc/spec":   mustProto(t, v.spec),
		"/volumes/vol-abc/status": mustProto(t, v.st),
	}}
	opts, stop := serveCLI(t, fs)
	defer stop()

	cmd := newVolumeCmd(opts)
	cmd.SetArgs([]string{"resync", "db"}) // no --replica
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--replica") {
		t.Fatalf("want --replica required error, got %v", err)
	}

	cmd = newVolumeCmd(opts)
	cmd.SetArgs([]string{"resync", "db", "--replica", "n2", "--full"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	op := string(fs.kv["/volumes/_ops/resync/vol-abc"])
	if !strings.Contains(op, `"replica":"n2"`) || !strings.Contains(op, `"full":true`) {
		t.Errorf("resync op wrong: %s", op)
	}
}

// TestDivergedListAndChoose: §9's human-in-the-loop valve.
func TestDivergedListAndChoose(t *testing.T) {
	v := fixtureVolume(t)
	v.st.State = pb.VolumeState_VOLUME_STATE_NEEDS_MANUAL_RECOVERY
	fs := &fakeStore{kv: map[string][]byte{
		"/volumes/vol-abc/spec":   mustProto(t, v.spec),
		"/volumes/vol-abc/status": mustProto(t, v.st),
	}}
	opts, stop := serveCLI(t, fs)
	defer stop()

	// Listing form.
	cmd := newVolumeCmd(opts)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"diverged"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "db") || !strings.Contains(buf.String(), "--choose") {
		t.Errorf("diverged listing wrong:\n%s", buf.String())
	}

	// Choose form writes the recover op.
	cmd = newVolumeCmd(opts)
	cmd.SetArgs([]string{"diverged", "db", "--choose", "n2"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	op := string(fs.kv["/volumes/_ops/recover/vol-abc"])
	if !strings.Contains(op, `"choose":"n2"`) {
		t.Errorf("recover op wrong: %s (keys %v)", op, fs.kv)
	}
}

func mustProto(t *testing.T, m pbproto.Message) []byte {
	t.Helper()
	b, err := pbproto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var _ = fmt.Sprint // keep fmt when test bodies evolve
