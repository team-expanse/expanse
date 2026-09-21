package main

import (
	"bytes"
	"context"
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
			State:   pb.VolumeState_VOLUME_STATE_HEALTHY,
			Primary: "n1",
			Placement: []*pb.Replica{
				{NodeId: "n1", Role: pb.ReplicaRole_REPLICA_ROLE_PRIMARY, Healthy: true, LastSeenUnixNano: time.Now().UnixNano()},
				{NodeId: "n2", Role: pb.ReplicaRole_REPLICA_ROLE_SECONDARY, Healthy: true, LastSeenUnixNano: time.Now().UnixNano()},
				{NodeId: "n3", Role: pb.ReplicaRole_REPLICA_ROLE_STALE},
			},
		},
	}
}

func TestPrintInspectShowsEveryReplicaWithRoleAndHealth(t *testing.T) {
	var buf bytes.Buffer
	printInspect(&buf, fixtureVolume(t))
	out := buf.String()
	for _, want := range []string{
		"volume db (vol-abc)", "state:    Healthy", "primary:  n1",
		"n1", "primary   ", "n2", "secondary", "n3", "stale", "true", "false", "never",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect output missing %q:\n%s", want, out)
		}
	}
}

func TestVolumeCommandSurface(t *testing.T) {
	cmd := newVolumeCmd(&ctlOpts{})
	want := map[string][]string{
		"create":       {"size", "class", "replication"},
		"list":         {},
		"delete":       {},
		"inspect":      {},
		"move-primary": {"to"},
		"diverged":     {},
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
}

func TestRetiredSubcommandsAreGone(t *testing.T) {
	cmd := newVolumeCmd(&ctlOpts{})
	for _, name := range []string{"resync", "verify", "snapshot", "restore", "resize"} {
		if sub, _, _ := cmd.Find([]string{name}); sub != nil && sub.Name() == name {
			t.Errorf("%s is still registered but nothing implements it", name)
		}
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
	if !strings.Contains(buf.String(), "primary:  n1") || !strings.Contains(buf.String(), "n2") {
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

func TestDivergedListsOnlyVolumesNeedingManualRecovery(t *testing.T) {
	bad, ok := fixtureVolume(t), fixtureVolume(t)
	bad.st.State = pb.VolumeState_VOLUME_STATE_NEEDS_MANUAL_RECOVERY
	bad.spec.Name = "split"
	fs := &fakeStore{kv: map[string][]byte{
		"/volumes/vol-bad/spec":   mustProto(t, bad.spec),
		"/volumes/vol-bad/status": mustProto(t, bad.st),
		"/volumes/vol-ok/spec":    mustProto(t, ok.spec),
		"/volumes/vol-ok/status":  mustProto(t, ok.st),
	}}
	opts, stop := serveCLI(t, fs)
	defer stop()

	cmd := newVolumeCmd(opts)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"diverged"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "split") || strings.Contains(out, "db") {
		t.Errorf("diverged listing wrong:\n%s", out)
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
