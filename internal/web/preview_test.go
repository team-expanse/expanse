package web

// A design-preview server, not a test: skipped unless EXPANSE_UI_PREVIEW
// names a listen address. It serves every page over plain HTTP with
// realistic fakes (a 3-node cluster with one unreachable node, degraded
// and failed volumes, blocks in several phases, alerts, OIDC configured)
// and no login, so headless-browser screenshots can be taken:
//
//	EXPANSE_UI_PREVIEW=127.0.0.1:8480 go test ./internal/web -run TestUIPreview -timeout 0
//
// ?theme=dark on any URL forces the manual dark theme, mirroring the
// header toggle, for screenshots without a dark OS setting; ?nosse=1
// disables the live connections so a screenshot tool sees the page
// reach "load".

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/expanse/expanse/internal/blocks/catalog"
	"github.com/expanse/expanse/internal/blocks/service"
	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/cluster/nodelc"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
	"github.com/expanse/expanse/internal/web/auth"
	"github.com/expanse/expanse/internal/web/oidc"
	pb "github.com/expanse/expanse/proto"
)

// previewCluster fakes NodeService for a 3-node cluster: n1 (serving,
// leader), n2 (voter), n3 (unreachable), plus local inventory/health.
type previewCluster struct {
	pb.UnimplementedNodeServiceServer
}

func (previewCluster) GetClusterStatus(context.Context, *pb.GetClusterStatusRequest) (*pb.GetClusterStatusResponse, error) {
	rep := control.Report{
		ClusterID: "c-7f3a9e12", Name: "lab-west", Version: "1.1.5", Leader: "10.20.0.11:7447", Generation: 14,
		QuorumNeed: 2, QuorumHave: 2,
		Nodes: []control.NodeStatus{
			{ID: "n1", RaftAddr: "10.20.0.11:7447", APIAddr: "10.20.0.11:7443", Role: "voter", State: "leader"},
			{ID: "n2", RaftAddr: "10.20.0.12:7447", APIAddr: "10.20.0.12:7443", Role: "voter", State: "voter"},
			{ID: "n3", RaftAddr: "10.20.0.13:7447", APIAddr: "10.20.0.13:7443", Role: "voter", State: "voter/unreachable", Lifecycle: nodelc.StateUnreachable},
		},
	}
	b, _ := json.Marshal(rep)
	return &pb.GetClusterStatusResponse{ReportJson: b}, nil
}

func (previewCluster) GetHealth(context.Context, *pb.GetHealthRequest) (*pb.HealthReport, error) {
	return &pb.HealthReport{Overall: pb.HealthReport_OVERALL_DEGRADED, Checks: []*pb.CheckResult{
		{Name: "disk-space", Status: pb.Health_HEALTH_HEALTHY, Message: "/ 41% used, /persist 63% used", TookMs: 2},
		{Name: "memory", Status: pb.Health_HEALTH_UNHEALTHY, Message: "swap 98% used; 412 MiB available", TookMs: 1},
		{Name: "raft", Status: pb.Health_HEALTH_HEALTHY, Message: "leader, 2/3 peers reachable", TookMs: 0},
		{Name: "time-sync", Status: pb.Health_HEALTH_DEGRADED, Message: "offset 180ms from peers", TookMs: 3},
	}}, nil
}

func (previewCluster) GetInventory(context.Context, *pb.GetInventoryRequest) (*pb.Inventory, error) {
	return &pb.Inventory{
		NodeId: "n1", Hostname: "rack1-a", Virtualization: "none",
		Os:     &pb.OSInfo{Name: "NixOS", Version: "25.05 (Warbler)", Kernel: "6.12.19", SystemClosurePath: "/nix/store/9k2m...-nixos-system-rack1-a-25.05"},
		Cpu:    &pb.CPUInfo{Model: "AMD EPYC 7302P 16-Core Processor", Cores: 16, Threads: 32, Mhz: 3000},
		Memory: &pb.MemoryInfo{Total: 128 << 30, Available: 51 << 30, SwapTotal: 8 << 30},
		Disks: []*pb.DiskInfo{
			{Path: "/dev/nvme0n1", Model: "Samsung PM9A3", Serial: "S5XYNE0R", Size: 960 << 30},
			{Path: "/dev/nvme1n1", Model: "Samsung PM9A3", Serial: "S5XYNE0T", Size: 960 << 30},
			{Path: "/dev/sda", Model: "WDC WUH721816AL", Serial: "2CG9ZAKY", Size: 16 << 40, Rotational: true},
		},
		Nics: []*pb.NICInfo{
			{Name: "eno1", Mac: "3c:ec:ef:12:34:56", Speed: 10000, Addresses: []string{"10.20.0.11/24", "fe80::3eec:efff:fe12:3456/64"}, Up: true},
			{Name: "eno2", Mac: "3c:ec:ef:12:34:57", Speed: 10000, Up: false},
		},
		Gpus:         []*pb.GPUInfo{{Vendor: "NVIDIA", Model: "A2", Vram: 16384, Driver: "nvidia 570.86", PciId: "0000:41:00.0"}},
		Tpm:          &pb.TPMInfo{Present: true, Version: "2.0"},
		Capabilities: []string{"storage", "gpu", "kvm"},
	}, nil
}

func (previewCluster) GetStatus(context.Context, *pb.GetStatusRequest) (*pb.NodeStatus, error) {
	return &pb.NodeStatus{NodeId: "n1", Status: pb.NodeStatus_STATUS_IDLE, TickCount: 8412, LastTickUnixNs: time.Now().Add(-7 * time.Second).UnixNano(), LastTickDurationMs: 12.4, ResourcesTotal: 23, ChangesApplied: 311, Failures: 2}, nil
}

func (previewCluster) ListResources(context.Context, *pb.ListResourcesRequest) (*pb.ListResourcesResponse, error) {
	return &pb.ListResourcesResponse{Resources: []*pb.Resource{
		{Id: "file:/etc/expanse/motd", Type: "file", Health: pb.Health_HEALTH_HEALTHY},
		{Id: "service:expanse-block-web", Type: "service", Health: pb.Health_HEALTH_HEALTHY},
		{Id: "service:expanse-block-cache", Type: "service", Health: pb.Health_HEALTH_UNHEALTHY},
		{Id: "vip:10.20.0.100", Type: "vip", Health: pb.Health_HEALTH_HEALTHY},
	}}, nil
}

func previewGens() []*pb.GenerationInfo {
	base := time.Now().Add(-9 * 24 * time.Hour)
	var out []*pb.GenerationInfo
	for i := uint64(1); i <= 14; i++ {
		g := &pb.GenerationInfo{Number: i, Revision: 100 + i*37, CreatedAtUnixNs: base.Add(time.Duration(i) * 15 * time.Hour).UnixNano(), CreatedBy: "system", Parent: i - 1, Hash: fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprint(i))))}
		if i == 9 {
			g.CreatedBy, g.Description = "user", "rollback to generation 7"
		}
		out = append(out, g)
	}
	return out
}

func (previewCluster) ListGenerations(context.Context, *pb.ListGenerationsRequest) (*pb.ListGenerationsResponse, error) {
	return &pb.ListGenerationsResponse{Generations: previewGens()}, nil
}

func (previewCluster) GetGeneration(_ context.Context, req *pb.GetGenerationRequest) (*pb.GenerationInfo, error) {
	for _, g := range previewGens() {
		if g.Number == req.Number {
			g.Keys = []string{"/blocks/default/web", "/blocks/default/cache", "/blocks/media/jellyfin", "/volumes/vol-01/spec", "/volumes/vol-02/spec", "/volumes/vol-03/spec", "/node/n1/resources/file:/etc/expanse/motd", "/node/n2/resources/file:/etc/expanse/motd"}
			return g, nil
		}
	}
	return nil, fmt.Errorf("no such generation")
}

func (previewCluster) DiffGenerations(context.Context, *pb.DiffGenerationsRequest) (*pb.DiffGenerationsResponse, error) {
	return &pb.DiffGenerationsResponse{
		Added:   []string{"/blocks/media/jellyfin", "/volumes/vol-03/spec"},
		Removed: []string{"/blocks/default/legacy-api"},
		Changed: []string{"/blocks/default/web", "/node/n1/resources/file:/etc/expanse/motd"},
	}, nil
}

// previewBlocks fakes BlockService with blocks in several phases.
type previewBlocks struct {
	pb.UnimplementedBlockServiceServer
}

func previewBlockList() []*pb.Block {
	mk := func(ns, name, typ string, want, ready int32, phase pb.Phase, msg string, placements ...*pb.PlacementStatus) *pb.Block {
		return &pb.Block{
			Metadata: &pb.Metadata{Namespace: ns, Name: name},
			Spec:     &pb.BlockSpec{Type: typ, Replicas: &want},
			Status:   &pb.BlockStatus{Phase: phase, Message: msg, ObservedGeneration: 14, Replicas: &pb.StatusReplicas{Desired: want, Ready: ready}, Placements: placements},
		}
	}
	return []*pb.Block{
		mk("default", "web", "web/nginx", 3, 3, pb.Phase_RUNNING, "", &pb.PlacementStatus{ReplicaIndex: 0, NodeId: "n1", Phase: pb.Phase_RUNNING, Generation: 14}, &pb.PlacementStatus{ReplicaIndex: 1, NodeId: "n2", Phase: pb.Phase_RUNNING, Generation: 14}, &pb.PlacementStatus{ReplicaIndex: 2, NodeId: "n3", Phase: pb.Phase_RUNNING, Generation: 13}),
		mk("default", "cache", "data/redis", 2, 1, pb.Phase_DEGRADED, "replica 1 lost: node n3 unreachable", &pb.PlacementStatus{ReplicaIndex: 0, NodeId: "n1", Phase: pb.Phase_RUNNING, Generation: 14}, &pb.PlacementStatus{ReplicaIndex: 1, NodeId: "n3", Phase: pb.Phase_LOST, Generation: 14}),
		mk("media", "jellyfin", "media/jellyfin", 1, 0, pb.Phase_PENDING, "", &pb.PlacementStatus{ReplicaIndex: 0, NodeId: "n2", Phase: pb.Phase_PROVISIONING, Generation: 14}),
		mk("default", "postgres", "data/postgres-ha", 2, 2, pb.Phase_UPDATING, "rolling update 13 → 14", &pb.PlacementStatus{ReplicaIndex: 0, NodeId: "n1", Phase: pb.Phase_RUNNING, Generation: 14}, &pb.PlacementStatus{ReplicaIndex: 1, NodeId: "n2", Phase: pb.Phase_DRAINING, Generation: 13}),
		mk("ops", "backup", "util/echo", 1, 0, pb.Phase_FAILED, "container exited with status 1"),
		previewProbedBlock(),
	}
}

// previewProbedBlock shows replica health: one ready, one restarting, one stopped after failing liveness.
func previewProbedBlock() *pb.Block {
	ago := func(d time.Duration) int64 { return time.Now().Add(-d).UnixNano() }
	want := int32(2)
	return &pb.Block{
		Metadata: &pb.Metadata{Namespace: "default", Name: "api"},
		Spec:     &pb.BlockSpec{Type: "web/nginx", Replicas: &want},
		Status: &pb.BlockStatus{
			Phase: pb.Phase_DEGRADED, ObservedGeneration: 14, Replicas: &pb.StatusReplicas{Desired: 2, Ready: 1},
			PendingReason: &pb.PendingReason{Code: "LivenessFailed", Message: "replica 1 failed its liveness probe on 2 nodes and was stopped"},
			Placements: []*pb.PlacementStatus{
				{ReplicaIndex: 0, NodeId: "n1", Phase: pb.Phase_RUNNING, Generation: 14, Health: &pb.ReplicaHealth{
					Readiness: &pb.ProbeResult{Ok: true, Detail: "HTTP 200", AtUnixNs: ago(20 * time.Second)},
					Liveness:  &pb.ProbeResult{Ok: true, Detail: "HTTP 500", AtUnixNs: ago(3 * time.Hour)}, Restarts: 1,
				}},
				{ReplicaIndex: -1, FormerIndex: 1, NodeId: "n2", Phase: pb.Phase_FAILED, Generation: 14, Message: "liveness probe failed after 5 restarts: dial tcp 10.0.0.2:8080: connect: connection refused"},
				{ReplicaIndex: -1, FormerIndex: 1, NodeId: "n3", Phase: pb.Phase_FAILED, Generation: 14, Message: "liveness probe failed after 5 restarts: dial tcp 10.0.0.3:8080: connect: connection refused"},
			},
		},
	}
}

func (previewBlocks) List(context.Context, *pb.ListBlocksRequest) (*pb.ListBlocksResponse, error) {
	return &pb.ListBlocksResponse{Blocks: previewBlockList()}, nil
}

func (previewBlocks) Get(_ context.Context, req *pb.GetBlockRequest) (*pb.Block, error) {
	for _, b := range previewBlockList() {
		if b.Metadata.Namespace == req.Namespace && b.Metadata.Name == req.Name {
			if req.Name == "jellyfin" {
				b.Status.PendingReason = &pb.PendingReason{Code: "InsufficientCapacity", Message: "no node has 8 GiB free for the requested memory", PerNode: map[string]string{"n1": "memory: 6.2 GiB free", "n3": "unreachable"}}
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("no such block")
}

func (previewBlocks) Watch(_ *pb.WatchBlocksRequest, s pb.BlockService_WatchServer) error {
	<-s.Context().Done()
	return nil
}

func (previewBlocks) StreamLogs(req *pb.LogsRequest, s pb.BlockService_StreamLogsServer) error {
	lines := []string{
		"2026-09-27T16:02:11Z nginx: [notice] using the \"epoll\" event method",
		"2026-09-27T16:02:11Z nginx: [notice] start worker processes",
		"10.20.0.7 - - [27/Sep/2026:16:02:19 +0000] \"GET / HTTP/1.1\" 200 612",
		"10.20.0.7 - - [27/Sep/2026:16:02:20 +0000] \"GET /healthz HTTP/1.1\" 200 2",
		"10.20.0.9 - - [27/Sep/2026:16:02:31 +0000] \"POST /api/upload HTTP/1.1\" 413 0",
	}
	for i, l := range lines {
		if err := s.Send(&pb.LogLine{Line: l, ReplicaIndex: req.Replica, TimestampUnixNs: time.Now().Add(time.Duration(i) * time.Second).UnixNano()}); err != nil {
			return err
		}
	}
	<-s.Context().Done()
	return nil
}

var _ grpc.ServerStream = (*blockEventStream)(nil)

// TestUIPreview serves the preview until interrupted.
func TestUIPreview(t *testing.T) {
	addr := os.Getenv("EXPANSE_UI_PREVIEW")
	if addr == "" {
		t.Skip("set EXPANSE_UI_PREVIEW=host:port to serve the design preview")
	}
	dir := t.TempDir()
	st, err := boltstore.New(filepath.Join(dir, "preview.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := auth.EnsureAdmin(ctx, st); err != nil {
		t.Fatal(err)
	}
	seedPreviewStore(t, st)

	cat, err := catalog.Load("../../nix/blocks")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New("n1", st, previewBlocks{}, service.NewCatalogServer(cat), previewCluster{}, clusterSecretForTest)
	if err != nil {
		t.Fatal(err)
	}
	s.DataDir = "/persist/expanse"
	seedPreviewEvents(s.events)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.events.run(runCtx, st)

	sess, err := auth.IssueSession(ctx, st, auth.AdminUsername)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Addr: addr, Handler: previewAuth(s.mux, sess.ID)}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		<-sig
		_ = srv.Close()
	}()
	fmt.Printf("preview: http://%s/  (Ctrl-C to stop)\n", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		t.Fatal(err)
	}
}

// previewAuth injects the pre-issued session on every request (no login
// needed for screenshots) and honours ?theme=dark by stamping the html
// element the way the header toggle does.
func previewAuth(next http.Handler, sessionID string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie(sessionCookie); err != nil && !strings.HasPrefix(r.URL.Path, "/login") {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionID})
		}
		dark, nosse := r.URL.Query().Get("theme") == "dark", r.URL.Query().Get("nosse") != ""
		if !dark && !nosse {
			next.ServeHTTP(w, r)
			return
		}
		rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		body := rec.body.Bytes()
		if dark {
			body = bytes.Replace(body, []byte(`<html lang="en">`), []byte(`<html lang="en" data-theme="dark">`), 1)
		}
		if nosse {
			body = bytes.ReplaceAll(body, []byte(`sse-connect="`), []byte(`data-sse-off="`))
		}
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(rec.status)
		_, _ = w.Write(body)
	})
}

type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) WriteHeader(code int)        { b.status = code }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }

func seedPreviewStore(t *testing.T, st store.Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	nodes := []join.NodeRecord{
		{ID: "n1", RaftAddr: "10.20.0.11:7447", APIAddr: "10.20.0.11:7443", Role: "voter", JoinedAt: now.Add(-40 * 24 * time.Hour).UnixNano()},
		{ID: "n2", RaftAddr: "10.20.0.12:7447", APIAddr: "10.20.0.12:7443", Role: "voter", JoinedAt: now.Add(-39 * 24 * time.Hour).UnixNano()},
		{ID: "n3", RaftAddr: "10.20.0.13:7447", APIAddr: "10.20.0.13:7443", Role: "voter", JoinedAt: now.Add(-12 * 24 * time.Hour).UnixNano(), State: nodelc.StateUnreachable},
	}
	status := map[string]string{"n1": "health=degraded", "n2": "health=healthy", "n3": "health=healthy"}
	for _, n := range nodes {
		b, _ := json.Marshal(n)
		if _, err := st.Put(ctx, store.Key(join.NodesKeyPrefix+n.ID), b); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Put(ctx, store.Key(join.NodesKeyPrefix+n.ID+"/status"), []byte(status[n.ID])); err != nil {
			t.Fatal(err)
		}
	}
	vols := []struct {
		spec   storage.Spec
		status storage.Status
		snaps  []storage.SnapshotRecord
	}{
		{storage.Spec{ID: "vol-01", Name: "postgres-data", SizeBytes: 200 << 30, Class: "default", Replication: 3},
			storage.Status{State: storage.StateHealthy, Primary: "n1", Placement: []storage.Replica{
				{NodeID: "n1", Role: storage.RolePrimary, Healthy: true, LastSeen: now.Add(-4 * time.Second)},
				{NodeID: "n2", Role: storage.RoleSecondary, Healthy: true, LastSeen: now.Add(-6 * time.Second)},
				{NodeID: "n3", Role: storage.RoleSecondary, Healthy: true, LastSeen: now.Add(-3 * time.Minute)},
			}},
			[]storage.SnapshotRecord{{Name: "nightly-0926", Node: "n1", CreatedAt: now.Add(-20 * time.Hour)}, {Name: "before-upgrade", Node: "n1", CreatedAt: now.Add(-5 * 24 * time.Hour)}}},
		{storage.Spec{ID: "vol-02", Name: "media", SizeBytes: 2 << 40, Class: "bulk", Replication: 2},
			storage.Status{State: storage.StateResyncing, Primary: "n2", Placement: []storage.Replica{
				{NodeID: "n2", Role: storage.RolePrimary, Healthy: true, LastSeen: now.Add(-2 * time.Second)},
				{NodeID: "n1", Role: storage.RoleSecondary, Healthy: true, SyncPercent: 43.5, OutOfSyncKiB: 812_000_000, LastSeen: now.Add(-2 * time.Second)},
			}}, nil},
		{storage.Spec{ID: "vol-03", Name: "cache", SizeBytes: 40 << 30, Class: "default", Replication: 2},
			storage.Status{State: storage.StateDegraded, Primary: "n1", Placement: []storage.Replica{
				{NodeID: "n1", Role: storage.RolePrimary, Healthy: true, LastSeen: now.Add(-1 * time.Second)},
				{NodeID: "n3", Role: storage.RoleSecondary, Healthy: false, OutOfSyncKiB: 5_120_000, LastSeen: now.Add(-14 * time.Minute)},
			}}, nil},
		{storage.Spec{ID: "vol-04", Name: "scratch", SizeBytes: 10 << 30, Class: "default", Replication: 3},
			storage.Status{State: storage.StateUnderReplicated, Primary: "n2", Placement: []storage.Replica{
				{NodeID: "n2", Role: storage.RolePrimary, Healthy: true, LastSeen: now.Add(-1 * time.Second)},
			}}, nil},
		{storage.Spec{ID: "vol-05", Name: "legacy", SizeBytes: 5 << 30, Class: "default", Replication: 2},
			storage.Status{State: storage.StateFailed, Primary: ""}, nil},
	}
	for _, v := range vols {
		if err := storage.SaveSpec(ctx, st, v.spec); err != nil {
			t.Fatal(err)
		}
		if err := storage.SaveStatus(ctx, st, v.spec.ID, v.status); err != nil {
			t.Fatal(err)
		}
		for _, sn := range v.snaps {
			if err := storage.PutSnapshot(ctx, st, v.spec.ID, sn); err != nil {
				t.Fatal(err)
			}
		}
	}
	rec, err := oidc.NewConfigRecord("https://sso.example.internal/realms/lab", "expanse-ui", "s3cret", "https://expanse-ui:8443/login/oidc/callback", []string{"jared@example.internal", "ops@example.internal"}, clusterSecretForTest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key(oidc.ConfigKey), rec); err != nil {
		t.Fatal(err)
	}
	ca, _ := json.Marshal(control.CAEntry{CertPEM: []byte("-----BEGIN CERTIFICATE-----\nMIIBpreview\n-----END CERTIFICATE-----\n"), SealedKey: []byte("sealed")})
	if _, err := st.Put(ctx, store.Key(control.UICAKey), ca); err != nil {
		t.Fatal(err)
	}
}

func seedPreviewEvents(l *eventLog) {
	keys := []string{"/nodes/n3/status", "/volumes/vol-02/status", "/blocks/default/cache", "/leases/block/default/postgres/singleton", "/cluster/generation", "/node/n1/resources/service:expanse-block-cache/status", "/ui/sessions/abcd1234", "/volumes/vol-03/status", "/blocks/media/jellyfin", "/nodes/n1/status"}
	for i, k := range keys {
		kind, shown := classifyEventKey(k)
		verb := "put"
		if i == 3 {
			verb = "delete"
		}
		l.add(eventEntry{At: time.Now().Add(-time.Duration(len(keys)-i) * 47 * time.Second), Verb: verb, Kind: kind, Key: shown})
	}
}
