package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/expanse/expanse/internal/testsock"
	pb "github.com/expanse/expanse/proto"
)

// startTestAgent runs a real agent (store, reconciler, managers, gRPC
// server) against a temp dir and returns a connected client plus a stop
// function. This exercises the full Phase 02 stack in-process.
func startTestAgent(t *testing.T) (pb.NodeServiceClient, func()) {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		NodeID:  "test-node",
		DataDir: dir,
		Socket:  testsock.Path(t, "agent.sock"),
		Period:  10 * time.Second, // slow: tests drive ticks explicitly
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runErr := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { runErr <- a.Run(ctx) }()

	// Wait for the socket.
	socket := cfg.Socket
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("agent socket never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	conn, err := grpc.NewClient("unix://"+filepath.ToSlash(socket),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		cancel()
		t.Fatalf("dial: %v", err)
	}
	cleanup := func() {
		conn.Close()
		cancel()
		select {
		case err := <-runErr:
			if err != nil {
				t.Errorf("agent run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("agent did not stop within 5s")
		}
	}
	return pb.NewNodeServiceClient(conn), cleanup
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestAgentEndToEnd(t *testing.T) {
	c, cleanup := startTestAgent(t)
	defer cleanup()
	ctx := context.Background()

	// GetStatus works and reports our node id.
	st, err := c.GetStatus(ctx, &pb.GetStatusRequest{})
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if st.NodeId != "test-node" {
		t.Errorf("node id = %q", st.NodeId)
	}

	// GetInventory: collected at startup.
	inv, err := c.GetInventory(ctx, &pb.GetInventoryRequest{})
	if err != nil {
		t.Fatalf("GetInventory: %v", err)
	}
	if inv.Cpu.GetCores() <= 0 {
		t.Errorf("cpu cores = %d", inv.Cpu.GetCores())
	}

	// GetHealth: checks ran.
	rep, err := c.GetHealth(ctx, &pb.GetHealthRequest{})
	if err != nil {
		t.Fatalf("GetHealth: %v", err)
	}
	if len(rep.Checks) == 0 {
		t.Error("no health checks in report")
	}

	// ApplyResources: a file resource.
	target := filepath.Join(t.TempDir(), "motd")
	spec := []byte(fmt.Sprintf("file:%s:\n  type: file\n  path: %s\n  content: e2e\n  mode: \"0644\"\n", target, target))
	resp, err := c.ApplyResources(ctx, &pb.ApplyResourcesRequest{Spec: spec})
	if err != nil {
		t.Fatalf("ApplyResources: %v", err)
	}
	if resp.Applied != 1 || resp.Failed != 0 {
		t.Fatalf("apply: %+v", resp)
	}

	// The watch-triggered tick converges the file quickly.
	waitFor(t, "file convergence", 10*time.Second, func() bool {
		data, err := os.ReadFile(target)
		return err == nil && string(data) == "e2e"
	})

	// ListResources / GetResource see it, healthy.
	lr, err := c.ListResources(ctx, &pb.ListResourcesRequest{})
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(lr.Resources) != 1 || lr.Resources[0].Id != "file:"+target {
		t.Fatalf("list: %+v", lr.Resources)
	}
	gr, err := c.GetResource(ctx, &pb.GetResourceRequest{Id: "file:" + target})
	if err != nil {
		t.Fatalf("GetResource: %v", err)
	}
	if gr.Health != pb.Health_HEALTH_HEALTHY {
		t.Errorf("resource health = %v, want healthy", gr.Health)
	}

	// Reconcile stream completes; idempotent (0 new changes).
	st1, _ := c.GetStatus(ctx, &pb.GetStatusRequest{})
	stream, err := c.Reconcile(ctx, &pb.ReconcileRequest{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var last *pb.ReconcileEvent
	for {
		ev, err := stream.Recv()
		if err != nil {
			t.Fatalf("reconcile stream: %v", err)
			break
		}
		last = ev
		if ev.Phase == "complete" {
			break
		}
	}
	if last == nil || last.Phase != "complete" {
		t.Fatalf("reconcile stream ended without complete: %+v", last)
	}
	st2, _ := c.GetStatus(ctx, &pb.GetStatusRequest{})
	if st2.ChangesApplied != st1.ChangesApplied {
		t.Errorf("in-sync reconcile applied changes: %d -> %d",
			st1.ChangesApplied, st2.ChangesApplied)
	}

	// StreamEvents: watch fires on a store write (the delete below).
	evStream, err := c.StreamEvents(ctx, &pb.StreamEventsRequest{})
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}

	// DeleteResource: removes the file and desired state.
	dr, err := c.DeleteResource(ctx, &pb.DeleteResourceRequest{Id: "file:" + target})
	if err != nil {
		t.Fatalf("DeleteResource: %v", err)
	}
	if !dr.Deleted {
		t.Fatal("DeleteResource reported not-deleted")
	}
	waitFor(t, "file removal after delete", 10*time.Second, func() bool {
		_, err := os.Stat(target)
		return os.IsNotExist(err)
	})

	// The events stream must have delivered the delete event.
	gotDelete := false
	deadline := time.After(5 * time.Second)
	for !gotDelete {
		select {
		case <-deadline:
			t.Fatal("no delete event on StreamEvents")
		default:
		}
		ev, err := evStream.Recv()
		if err != nil {
			t.Fatalf("events stream: %v", err)
		}
		if ev.Type == "delete" {
			gotDelete = true
		}
	}
}

func TestAgentApplySpecValidation(t *testing.T) {
	c, cleanup := startTestAgent(t)
	defer cleanup()
	ctx := context.Background()

	// Empty spec rejected.
	if _, err := c.ApplyResources(ctx, &pb.ApplyResourcesRequest{}); err == nil {
		t.Error("empty spec must be rejected")
	}
	// Missing type fails that entry but not the call.
	resp, err := c.ApplyResources(ctx, &pb.ApplyResourcesRequest{Spec: []byte("file:/x:\n  path: /x\n")})
	if err != nil {
		t.Fatalf("ApplyResources: %v", err)
	}
	if resp.Failed != 1 || resp.Applied != 0 || len(resp.Errors) != 1 {
		t.Errorf("bad entry: %+v", resp)
	}
}

func TestAgentDryRunReconcile(t *testing.T) {
	c, cleanup := startTestAgent(t)
	defer cleanup()
	ctx := context.Background()

	target := filepath.Join(t.TempDir(), "never")
	spec := []byte(fmt.Sprintf("file:%s:\n  type: file\n  path: %s\n  content: no\n  mode: \"0644\"\n", target, target))
	if _, err := c.ApplyResources(ctx, &pb.ApplyResourcesRequest{Spec: spec}); err != nil {
		t.Fatalf("ApplyResources: %v", err)
	}
	// Dry-run reconcile must not create the file.
	stream, err := c.Reconcile(ctx, &pb.ReconcileRequest{DryRun: true})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if ev.Phase == "complete" {
			break
		}
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("dry-run reconcile created %s", target)
	}
	// Dry-run must reset: a normal reconcile now converges.
	stream, err = c.Reconcile(ctx, &pb.ReconcileRequest{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if ev.Phase == "complete" {
			break
		}
	}
	waitFor(t, "post-dry-run convergence", 10*time.Second, func() bool {
		_, err := os.Stat(target)
		return err == nil
	})
}

func TestVolumeStorageIsWiredOnlyWhenAVolumeGroupIsConfigured(t *testing.T) {
	newAgent := func(vg string) *Agent {
		t.Helper()
		dir := t.TempDir()
		a, err := New(Config{NodeID: "n1", DataDir: dir, Socket: testsock.Path(t, "a.sock"), StorageVG: vg})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	if a := newAgent(""); a.volnode != nil || a.volctl != nil {
		t.Error("volume storage wired without a volume group")
	}
	a := newAgent("vg0")
	if a.volnode == nil || a.volctl == nil {
		t.Fatal("volume storage not wired")
	}
	if a.cfg.DRBDConfigDir != defaultDRBDConfigDir {
		t.Errorf("config dir = %q, want the default", a.cfg.DRBDConfigDir)
	}
}

// slowStopper is a volume node whose shutdown (demoting its volumes) takes a while.
type slowStopper struct{ finished chan struct{} }

func (s *slowStopper) Run(ctx context.Context, _ time.Duration) {
	<-ctx.Done()
	time.Sleep(200 * time.Millisecond)
	close(s.finished)
}

func TestRunWaitsForTheVolumeNodeToStopBeforeReturning(t *testing.T) {
	dir := t.TempDir()
	a, err := New(Config{NodeID: "n1", DataDir: dir, Socket: testsock.Path(t, "a.sock"), StorageVG: "vg0"})
	if err != nil {
		t.Fatal(err)
	}
	node := &slowStopper{finished: make(chan struct{})}
	a.volnode = node
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	time.Sleep(300 * time.Millisecond) // let Run start the node
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run never returned")
	}
	select {
	case <-node.finished:
	default:
		t.Fatal("Run returned before the volume node finished stopping: volumes would stay primary")
	}
}
