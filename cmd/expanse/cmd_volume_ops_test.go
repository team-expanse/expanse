package main

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pbproto "google.golang.org/protobuf/proto"

	"github.com/expanse/expanse/internal/storage"
	pb "github.com/expanse/expanse/proto"
)

// fakeStore is a minimal NodeServiceServer backed by a map — enough
// surface for the volume CLI (Get/List/Put/Delete KeyValue).
type fakeStore struct {
	pb.UnimplementedNodeServiceServer
	kv   map[string][]byte
	puts []string // keys in the order they were written
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
	f.puts = append(f.puts, r.GetKey())
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
		"delete":       {"force"},
		"retire":       {"node"},
		"resize":       {"size"},
		"snapshot":     {"name"},
		"restore":      {"snapshot"},
		"inspect":      {},
		"move-primary": {"to"},
		"diverged":     {"choose"},
		"verify":       {},
		"resync":       {"node"},
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

func resizeStore(t *testing.T) *fakeStore {
	t.Helper()
	v := fixtureVolume(t)
	return &fakeStore{kv: map[string][]byte{
		"/volumes/vol-abc/spec":   mustProto(t, v.spec),
		"/volumes/vol-abc/status": mustProto(t, v.st),
	}}
}

func runResize(t *testing.T, fs *fakeStore, args ...string) error {
	t.Helper()
	return runVolume(t, fs, append([]string{"resize"}, args...)...)
}

func runVolume(t *testing.T, fs *fakeStore, args ...string) error {
	t.Helper()
	opts, stop := serveCLI(t, fs)
	defer stop()
	cmd := newVolumeCmd(opts)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	return cmd.Execute()
}

func TestResizeOpCarriesTheNewSizeInBytes(t *testing.T) {
	fs := resizeStore(t)
	if err := runResize(t, fs, "db", "--size", "20Gi"); err != nil {
		t.Fatal(err)
	}
	op, ok := fs.kv["/volumes/_ops/resize/vol-abc"]
	if !ok {
		t.Fatalf("resize op not written; keys: %v", fs.kv)
	}
	if want := `"sizeBytes":21474836480`; !strings.Contains(string(op), want) {
		t.Errorf("op payload %s lacks %s", op, want)
	}
}

func TestResizeRefusesAShrinkOrANoOpWithoutWritingAnOp(t *testing.T) {
	for _, size := range []string{"5Gi", "10Gi"} {
		fs := resizeStore(t)
		if err := runResize(t, fs, "db", "--size", size); err == nil {
			t.Errorf("--size %s: want an error, got none", size)
		}
		if _, ok := fs.kv["/volumes/_ops/resize/vol-abc"]; ok {
			t.Errorf("--size %s: an op was written for a request that is not a grow", size)
		}
	}
}

func TestResizeNeedsAValidSizeAndAKnownVolume(t *testing.T) {
	if err := runResize(t, resizeStore(t), "db"); err == nil {
		t.Error("missing --size: want an error")
	}
	if err := runResize(t, resizeStore(t), "db", "--size", "lots"); err == nil {
		t.Error("unparsable --size: want an error")
	}
	if err := runResize(t, resizeStore(t), "nope", "--size", "20Gi"); err == nil {
		t.Error("unknown volume: want an error")
	}
}

// snapStore is the fixture volume (primary n1) with the given snapshots recorded on nodes.
func snapStore(t *testing.T, held map[string]string) *fakeStore {
	t.Helper()
	fs := resizeStore(t)
	for name, node := range held {
		fs.kv["/volumes/vol-abc/snapshots/"+name] = []byte(`{"name":"` + name + `","node":"` + node + `"}`)
	}
	return fs
}

func TestSnapshotQueuesAnOpNamingTheSnapshot(t *testing.T) {
	fs := snapStore(t, nil)
	if err := runVolume(t, fs, "snapshot", "db", "--name", "before"); err != nil {
		t.Fatal(err)
	}
	if got, want := string(fs.kv["/volumes/_ops/snapshot/vol-abc"]), `"name":"before"`; !strings.Contains(got, want) {
		t.Errorf("op %q lacks %s", got, want)
	}
}

func TestSnapshotRefusesABadOrTakenNameWithoutQueueingAnything(t *testing.T) {
	for name, args := range map[string][]string{
		"no name":    {"snapshot", "db"},
		"bad name":   {"snapshot", "db", "--name", "Bad Name"},
		"taken name": {"snapshot", "db", "--name", "before"},
		"no volume":  {"snapshot", "nope", "--name", "x"},
	} {
		fs := snapStore(t, map[string]string{"before": "n1"})
		if err := runVolume(t, fs, args...); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if _, ok := fs.kv["/volumes/_ops/snapshot/vol-abc"]; ok {
			t.Errorf("%s: an op was queued", name)
		}
	}
}

func TestRestoreQueuesAnOpForASnapshotHeldByThePrimary(t *testing.T) {
	fs := snapStore(t, map[string]string{"before": "n1"})
	if err := runVolume(t, fs, "restore", "db", "--snapshot", "before"); err != nil {
		t.Fatal(err)
	}
	if got, want := string(fs.kv["/volumes/_ops/restore/vol-abc"]), `"name":"before"`; !strings.Contains(got, want) {
		t.Errorf("op %q lacks %s", got, want)
	}
}

func TestRestoreRefusesAnUnknownSnapshotOrOneHeldElsewhere(t *testing.T) {
	cases := map[string]struct {
		held map[string]string
		args []string
		want string
	}{
		"unknown":    {map[string]string{"other": "n1"}, []string{"restore", "db", "--snapshot", "before"}, "other"},
		"held by n2": {map[string]string{"before": "n2"}, []string{"restore", "db", "--snapshot", "before"}, "--to n2"},
		"no flag":    {map[string]string{"before": "n1"}, []string{"restore", "db"}, "--snapshot"},
	}
	for name, tc := range cases {
		fs := snapStore(t, tc.held)
		err := runVolume(t, fs, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want one mentioning %q", name, err, tc.want)
		}
		if _, ok := fs.kv["/volumes/_ops/restore/vol-abc"]; ok {
			t.Errorf("%s: an op was queued", name)
		}
	}
}

func TestInspectListsSnapshotsWithTheirHolders(t *testing.T) {
	v := fixtureVolume(t)
	v.snaps = []storage.SnapshotRecord{{Name: "before", Node: "n1", CreatedAt: time.Now().Add(-time.Hour)}}
	var buf bytes.Buffer
	printInspect(&buf, v)
	for _, want := range []string{"snapshots:", "before", "n1", "1h"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("inspect lacks %q:\n%s", want, buf.String())
		}
	}
}

func TestLoadVolumesAttachesSnapshotRecordsAndIgnoresUnknownKeys(t *testing.T) {
	fs := snapStore(t, map[string]string{"before": "n1"})
	fs.kv["/volumes/vol-abc/notes/x"] = []byte("?")
	opts, stop := serveCLI(t, fs)
	defer stop()
	conn, err := dial(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	vols, err := loadVolumes(context.Background(), pb.NewNodeServiceClient(conn))
	if err != nil {
		t.Fatal(err)
	}
	if got := vols["db"].snaps; len(got) != 1 || got[0].Name != "before" || got[0].Node != "n1" {
		t.Errorf("snaps %+v, want before on n1", got)
	}
}

func divergedStore(t *testing.T) *fakeStore {
	t.Helper()
	v := fixtureVolume(t)
	v.st.State = pb.VolumeState_VOLUME_STATE_NEEDS_MANUAL_RECOVERY
	return &fakeStore{kv: map[string][]byte{
		"/volumes/vol-abc/spec":   mustProto(t, v.spec),
		"/volumes/vol-abc/status": mustProto(t, v.st),
	}}
}

func TestChoosingASurvivorQueuesARequestPerReplicaWithTheSurvivorLast(t *testing.T) {
	fs := divergedStore(t)
	if err := runVolume(t, fs, "diverged", "db", "--choose", "n2"); err != nil {
		t.Fatal(err)
	}
	want := []string{"/volumes/_ops/resolve/vol-abc/n1", "/volumes/_ops/resolve/vol-abc/n3", "/volumes/_ops/resolve/vol-abc/n2"}
	if !reflect.DeepEqual(fs.puts, want) {
		t.Errorf("writes %v, want %v", fs.puts, want)
	}
	for _, k := range want {
		if got := string(fs.kv[k]); got != `{"survivor":"n2"}` {
			t.Errorf("%s = %s", k, got)
		}
	}
}

func TestChoosingSaysWhichReplicasLoseTheirChanges(t *testing.T) {
	opts, stop := serveCLI(t, divergedStore(t))
	defer stop()
	cmd := newVolumeCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"diverged", "db", "--choose", "n2"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"n2", "keeps", "n1", "n3", "discard"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestChoosingRefusesWhatTheVolumeCannotHonourAndQueuesNothing(t *testing.T) {
	healthy := resizeStore(t)
	for name, tc := range map[string]struct {
		fs   *fakeStore
		args []string
	}{
		"not diverged":          {healthy, []string{"diverged", "db", "--choose", "n1"}},
		"survivor not a member": {divergedStore(t), []string{"diverged", "db", "--choose", "n9"}},
		"unknown volume":        {divergedStore(t), []string{"diverged", "nope", "--choose", "n1"}},
		"no survivor named":     {divergedStore(t), []string{"diverged", "db"}},
		"no volume named":       {divergedStore(t), []string{"diverged", "--choose", "n1"}},
	} {
		if err := runVolume(t, tc.fs, tc.args...); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if len(tc.fs.puts) != 0 {
			t.Errorf("%s: wrote %v", name, tc.fs.puts)
		}
	}
}

// healthyStore is a volume whose three replicas are all in sync, n1 being the primary.
func healthyStore(t *testing.T, edit ...func(*pb.VolumeStatus)) *fakeStore {
	t.Helper()
	v := fixtureVolume(t)
	v.st.Placement[2] = &pb.Replica{NodeId: "n3", Role: pb.ReplicaRole_REPLICA_ROLE_SECONDARY, Healthy: true}
	for _, e := range edit {
		e(v.st)
	}
	return &fakeStore{kv: map[string][]byte{
		"/volumes/vol-abc/spec":   mustProto(t, v.spec),
		"/volumes/vol-abc/status": mustProto(t, v.st),
	}}
}

func TestVerifyQueuesOneRequestForThePrimaryToCarryOut(t *testing.T) {
	fs := healthyStore(t)
	opts, stop := serveCLI(t, fs)
	defer stop()
	cmd := newVolumeCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"verify", "db"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/volumes/_ops/verify/vol-abc"}; !reflect.DeepEqual(fs.puts, want) {
		t.Errorf("writes %v, want %v", fs.puts, want)
	}
	if !strings.Contains(out.String(), "n1") {
		t.Errorf("output does not say the primary runs it:\n%s", out.String())
	}
}

func TestResyncQueuesARequestForTheNamedReplicaAndSaysItIsRebuilt(t *testing.T) {
	fs := healthyStore(t)
	opts, stop := serveCLI(t, fs)
	defer stop()
	cmd := newVolumeCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"resync", "db", "--node", "n2"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/volumes/_ops/resync/vol-abc/n2"}; !reflect.DeepEqual(fs.puts, want) {
		t.Errorf("writes %v, want %v", fs.puts, want)
	}
	for _, want := range []string{"n2", "rebuilt", "discard"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestVerifyAndResyncRefuseAVolumeThatIsNotFullyInSyncAndQueueNothing(t *testing.T) {
	degraded := func(s *pb.VolumeStatus) { s.State = pb.VolumeState_VOLUME_STATE_DEGRADED }
	diverged := func(s *pb.VolumeStatus) { s.State = pb.VolumeState_VOLUME_STATE_NEEDS_MANUAL_RECOVERY }
	behind := func(s *pb.VolumeStatus) { s.Placement[1].Healthy = false }
	alone := func(s *pb.VolumeStatus) { s.Placement = s.Placement[:1] }
	for name, tc := range map[string]struct {
		fs   *fakeStore
		args []string
	}{
		"verify degraded":         {healthyStore(t, degraded), []string{"verify", "db"}},
		"verify diverged":         {healthyStore(t, diverged), []string{"verify", "db"}},
		"verify a replica behind": {healthyStore(t, behind), []string{"verify", "db"}},
		"verify one replica":      {healthyStore(t, alone), []string{"verify", "db"}},
		"verify unknown":          {healthyStore(t), []string{"verify", "nope"}},
		"resync degraded":         {healthyStore(t, degraded), []string{"resync", "db", "--node", "n2"}},
		"resync diverged":         {healthyStore(t, diverged), []string{"resync", "db", "--node", "n2"}},
		"resync a replica behind": {healthyStore(t, behind), []string{"resync", "db", "--node", "n3"}},
		"resync one replica":      {healthyStore(t, alone), []string{"resync", "db", "--node", "n1"}},
		"resync the primary":      {healthyStore(t), []string{"resync", "db", "--node", "n1"}},
		"resync a non-member":     {healthyStore(t), []string{"resync", "db", "--node", "n9"}},
		"resync no node":          {healthyStore(t), []string{"resync", "db"}},
		"resync unknown":          {healthyStore(t), []string{"resync", "nope", "--node", "n2"}},
	} {
		if err := runVolume(t, tc.fs, tc.args...); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if len(tc.fs.puts) != 0 {
			t.Errorf("%s: wrote %v", name, tc.fs.puts)
		}
	}
}

func TestResyncWithoutANodeSaysWhichFlagIsMissing(t *testing.T) {
	err := runVolume(t, healthyStore(t), "resync", "db")
	if err == nil || !strings.Contains(err.Error(), "--node") {
		t.Errorf("error %v, want one naming --node", err)
	}
}

// inspectRow is the whitespace-separated fields of a replica's line in `volume inspect`.
func inspectRow(t *testing.T, out, node string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == node {
			return f
		}
	}
	t.Fatalf("no row for %s:\n%s", node, out)
	return nil
}

func TestPrintInspectShowsSyncProgressVerifyStateAndWhatAVerifyFound(t *testing.T) {
	v := fixtureVolume(t)
	v.st.Placement = []*pb.Replica{
		{NodeId: "n1", Role: pb.ReplicaRole_REPLICA_ROLE_PRIMARY, Healthy: true},
		{NodeId: "n2", Role: pb.ReplicaRole_REPLICA_ROLE_SECONDARY, Healthy: true, OutOfSyncKib: 2048},
		{NodeId: "n3", Role: pb.ReplicaRole_REPLICA_ROLE_RESYNCING, SyncPercent: 43},
		{NodeId: "n4", Role: pb.ReplicaRole_REPLICA_ROLE_SECONDARY, Healthy: true, Verifying: true},
		{NodeId: "n5", Role: pb.ReplicaRole_REPLICA_ROLE_RESYNCING}, // just started
	}
	var buf bytes.Buffer
	printInspect(&buf, v)
	out := buf.String()
	for node, want := range map[string][2]string{ // node -> SYNC, OUT OF SYNC
		"n1": {"-", "-"}, "n2": {"-", "2Mi"}, "n3": {"43%", "-"}, "n4": {"verifying", "-"}, "n5": {"0%", "-"},
	} {
		if f := inspectRow(t, out, node); f[3] != want[0] || f[4] != want[1] {
			t.Errorf("%s: SYNC %q, OUT OF SYNC %q; want %q, %q\n%s", node, f[3], f[4], want[0], want[1], out)
		}
	}
	if !strings.Contains(out, "SYNC") || !strings.Contains(out, "OUT OF SYNC") {
		t.Errorf("no columns for progress:\n%s", out)
	}
}

func soloStore(t *testing.T) *fakeStore {
	t.Helper()
	return &fakeStore{kv: map[string][]byte{
		"/volumes/vol-solo/spec": mustProto(t, &pb.VolumeSpec{Name: "solo", SizeBytes: 1 << 30, Replication: 3}),
		"/volumes/vol-solo/status": mustProto(t, &pb.VolumeStatus{
			State: pb.VolumeState_VOLUME_STATE_UNDER_REPLICATED, Primary: "n1",
			Placement: []*pb.Replica{{NodeId: "n1", Role: pb.ReplicaRole_REPLICA_ROLE_PRIMARY, Healthy: true}},
		}),
		"/volumes/_pending/big":           mustProto(t, &pb.VolumeSpec{Name: "big", Replication: 3}),
		"/volume-placement-reasons/big":   []byte("needs 3 nodes, 1 eligible"),
		"/volume-placement-reasons/stale": []byte("left over from a placed volume"),
	}}
}

func runVolumeOut(t *testing.T, fs *fakeStore, args ...string) string {
	t.Helper()
	opts, stop := serveCLI(t, fs)
	defer stop()
	var out bytes.Buffer
	cmd := newVolumeCmd(opts)
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestListShowsMembersOfTargetAndWhyRequestsArePending(t *testing.T) {
	out := runVolumeOut(t, soloStore(t), "list")
	for _, want := range []string{"solo", "UnderReplicated", "1/3", "n1 (n1)  no redundancy", "big", "pending", "needs 3 nodes, 1 eligible"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "stale") {
		t.Errorf("list shows a reason with no pending request:\n%s", out)
	}
}

func TestInspectSaysAnUnderReplicatedVolumeHasNoRedundancy(t *testing.T) {
	out := runVolumeOut(t, soloStore(t), "inspect", "solo")
	if !strings.Contains(out, "replicas: 1 of 3 (no redundancy)") {
		t.Errorf("inspect output lacks the replica summary:\n%s", out)
	}
}

func TestCreateLeavesReplicationToTheClassUnlessGiven(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int32
	}{
		{[]string{"create", "a", "--size", "1Gi"}, 0},
		{[]string{"create", "a", "--size", "1Gi", "--replication", "2"}, 2},
	} {
		fs := &fakeStore{kv: map[string][]byte{}}
		runVolumeOut(t, fs, tc.args...)
		var spec pb.VolumeSpec
		if err := pbproto.Unmarshal(fs.kv["/volumes/_pending/a"], &spec); err != nil || spec.GetReplication() != tc.want {
			t.Errorf("%v: replication = %d, %v; want %d", tc.args, spec.GetReplication(), err, tc.want)
		}
	}
}

func TestCLIKeyPrefixesMatchTheStorageModel(t *testing.T) {
	if volumePendingKeyPrefix != storage.PendingPrefix || placementReasonPrefix != storage.PlacementReasonPrefix {
		t.Fatalf("CLI prefixes %q, %q drifted from storage's %q, %q",
			volumePendingKeyPrefix, placementReasonPrefix, storage.PendingPrefix, storage.PlacementReasonPrefix)
	}
}

func TestTwoInSyncReplicasOfThreeCanBeVerified(t *testing.T) {
	v := fixtureVolume(t)
	v.st.State = pb.VolumeState_VOLUME_STATE_UNDER_REPLICATED
	v.st.Placement = v.st.Placement[:2]
	if err := checkInSync(v); err != nil {
		t.Fatalf("checkInSync = %v; two in-sync replicas are enough to compare", err)
	}
}

func TestLoadVolumesAttachesTheTiebreakerFromTheDRBDAllocation(t *testing.T) {
	fs := snapStore(t, nil)
	fs.kv["/drbd/vol/vol-abc"] = []byte(`{"name":"vol-abc","nodeIDs":{"n1":0,"n2":1},"diskless":{"n3":2}}`)
	opts, stop := serveCLI(t, fs)
	defer stop()
	conn, err := dial(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	vols, err := loadVolumes(context.Background(), pb.NewNodeServiceClient(conn))
	if err != nil {
		t.Fatal(err)
	}
	if got := vols["db"].tiebreakers; len(got) != 1 || got[0] != "n3" {
		t.Errorf("tiebreakers %v, want [n3]", got)
	}
}

func TestInspectNamesTheTiebreakerOfATwoReplicaVolume(t *testing.T) {
	cases := map[string]struct {
		tiebreakers []string
		replication int32
		want        string
	}{
		"held":         {[]string{"n3"}, 2, "tiebreaker: n3"},
		"none":         {nil, 2, "tiebreaker: none"},
		"three copies": {nil, 3, ""},
	}
	for name, tc := range cases {
		v := fixtureVolume(t)
		v.spec.Replication = tc.replication
		v.tiebreakers = tc.tiebreakers
		var buf bytes.Buffer
		printInspect(&buf, v)
		got := buf.String()
		if tc.want == "" && strings.Contains(got, "tiebreaker") || tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s: inspect, want %q:\n%s", name, tc.want, got)
		}
	}
}
