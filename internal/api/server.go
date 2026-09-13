// Package api implements the agent's gRPC NodeService (proto/node.proto).
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/expanse/expanse/proto"

	"github.com/expanse/expanse/internal/agent/health"
	"github.com/expanse/expanse/internal/agent/inventory"
	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/generation"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/reconcile"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
)

// Agent is the narrow interface the server needs from the agent
// (implemented by internal/agent.Agent; keeps the dependency direction
// one-way: agent → api).
type Agent interface {
	NodeID() string
	// Snapshot is the live status summary for GetStatus.
	Snapshot() Snapshot
	// Inventory returns the last collected inventory.
	Inventory() *inventory.Inventory
	// Health runs the checks and returns the report.
	Health(ctx context.Context) *health.Report
	// Reconciler exposes the loop for metrics and manual ticks.
	Reconciler() *reconcile.Reconciler
	// ApplySpec decodes a YAML resource spec document and writes it as
	// desired state; returns (applied, failed, errors).
	ApplySpec(ctx context.Context, spec []byte) (int, int, []string)
	// DeleteResource removes a resource's desired state and, when the
	// manager supports it (reconcile.Deleter), the managed object.
	DeleteResource(ctx context.Context, id string) (bool, error)
	// Shutdown requests graceful agent shutdown.
	Shutdown(reason string)
}

// Snapshot is the live node status summary.
type Snapshot struct {
	Status         string
	ResourcesTotal int64
	ChangesApplied int64
	Failures       int64
	Ticks          int64
	LastTickAt     time.Time
	LastTickTookMs float64
	TickP99Ms      float64
}

// Server implements pb.NodeServiceServer.
type Server struct {
	pb.UnimplementedNodeServiceServer
	agent  Agent
	store  store.Store
	logger *slog.Logger
}

// NewServer creates the NodeService server.
func NewServer(agent Agent, s store.Store, logger *slog.Logger) *Server {
	return &Server{agent: agent, store: s, logger: logger.With("component", "api")}
}

// Serve starts the gRPC server on a unix socket (mode 0660; filesystem
// permissions are the auth) and blocks until ctx is done.
func (s *Server) Serve(ctx context.Context, socketPath string) error {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o750); err != nil {
		return fmt.Errorf("socket dir: %w", err)
	}
	_ = os.Remove(socketPath) // stale socket from a previous crash
	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o660); err != nil {
		_ = lis.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}
	// Group access for the expanse group when present.
	chownGroup(socketPath, "expanse")

	gs := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			unaryRecovery(s.logger),
			unaryTimeout(30*time.Second),
		),
		grpc.ChainStreamInterceptor(
			streamRecovery(s.logger),
		),
	)
	pb.RegisterNodeServiceServer(gs, s)
	s.logger.Info("grpc listening", "socket", socketPath)

	errCh := make(chan error, 1)
	go func() { errCh <- gs.Serve(lis) }()
	select {
	case <-ctx.Done():
		done := make(chan struct{})
		go func() { gs.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			gs.Stop()
		}
		_ = os.Remove(socketPath)
		return nil
	case err := <-errCh:
		return err
	}
}

func chownGroup(socketPath, group string) {
	gid, err := lookupGroup(group)
	if err != nil {
		return // group absent: root-only socket is safe
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(socketPath, -1, gid); err == nil {
			return
		}
	}
}

// ---- rpc methods ----

// GetStatus returns the current node status.
func (s *Server) GetStatus(ctx context.Context, req *pb.GetStatusRequest) (*pb.NodeStatus, error) {
	snap := s.agent.Snapshot()
	m := s.agent.Reconciler().Metrics()
	st := pb.NodeStatus_STATUS_IDLE
	switch snap.Status {
	case "reconciling":
		st = pb.NodeStatus_STATUS_RECONCILING
	case "degraded":
		st = pb.NodeStatus_STATUS_DEGRADED
	case "error":
		st = pb.NodeStatus_STATUS_ERROR
	}
	return &pb.NodeStatus{
		NodeId:             s.agent.NodeID(),
		Status:             st,
		TickCount:          m.Ticks,
		LastTickUnixNs:     m.LastTickAt.UnixNano(),
		LastTickDurationMs: m.LastTickTookMs,
		ResourcesTotal:     m.ResourcesTotal,
		ChangesApplied:     m.ChangesApplied,
		Failures:           m.Failures,
	}, nil
}

// GetInventory returns the system inventory.
func (s *Server) GetInventory(ctx context.Context, req *pb.GetInventoryRequest) (*pb.Inventory, error) {
	inv := s.agent.Inventory()
	if inv == nil {
		return nil, errors.New(errors.KindUnavailable, "GetInventory", "inventory not collected yet")
	}
	return inventoryToProto(inv), nil
}

func inventoryToProto(inv *inventory.Inventory) *pb.Inventory {
	p := &pb.Inventory{
		NodeId:         inv.NodeID,
		Hostname:       inv.Hostname,
		Virtualization: inv.Virtualization,
	}
	if inv.TPM != nil {
		p.Tpm = &pb.TPMInfo{Present: inv.TPM.Present, Version: inv.TPM.Version}
	}
	p.Capabilities = append(p.Capabilities, inv.Capabilities...)
	p.Os = &pb.OSInfo{
		Name:              inv.OS.Name,
		Version:           inv.OS.Version,
		Kernel:            inv.OS.Kernel,
		SystemClosurePath: inv.OS.SystemClosurePath,
	}
	p.Cpu = &pb.CPUInfo{
		Model:   inv.CPU.Model,
		Cores:   int64(inv.CPU.Cores),
		Threads: int64(inv.CPU.Threads),
		Mhz:     inv.CPU.MHz,
		Flags:   inv.CPU.Flags,
	}
	p.Memory = &pb.MemoryInfo{
		Total:     int64(inv.Memory.Total),
		Available: int64(inv.Memory.Available),
		SwapTotal: int64(inv.Memory.SwapTotal),
	}
	for _, d := range inv.Disks {
		p.Disks = append(p.Disks, &pb.DiskInfo{
			Path: d.Path, Model: d.Model, Serial: d.Serial,
			Size: int64(d.Size), Rotational: d.Rotational,
		})
	}
	for _, n := range inv.Network {
		p.Nics = append(p.Nics, &pb.NICInfo{
			Name: n.Name, Mac: n.MAC, Speed: int64(n.SpeedMbps),
			Addresses: n.Addresses, Up: n.Up,
		})
	}
	for _, g := range inv.GPUs {
		p.Gpus = append(p.Gpus, &pb.GPUInfo{
			Vendor: g.Vendor, Model: g.Model, Vram: g.VRAM,
			Driver: g.Driver, PciId: g.PCIID,
		})
	}
	return p
}

// ListResources returns desired-state entries under a prefix.
func (s *Server) ListResources(ctx context.Context, req *pb.ListResourcesRequest) (*pb.ListResourcesResponse, error) {
	prefix := store.Key(req.Prefix)
	if prefix == "" || prefix == "/" {
		prefix = s.agent.Reconciler().DesiredPrefix()
	}
	entries, err := s.store.List(ctx, prefix)
	if err != nil {
		return nil, mapErr("ListResources", err)
	}
	resp := &pb.ListResourcesResponse{}
	for _, e := range entries {
		id := strings.TrimPrefix(string(e.Key), string(s.agent.Reconciler().DesiredPrefix()))
		typ, spec := splitTypeHeader(e.Value)
		st, _ := s.store.Get(ctx, s.agent.Reconciler().StatusPrefix()+store.Key(id))
		resp.Resources = append(resp.Resources, &pb.Resource{
			Id:              id,
			Type:            typ,
			DesiredState:    string(spec),
			ObservedState:   observedState(st),
			Health:          healthOfStatus(st),
			UpdatedAtUnixNs: e.UpdatedAt,
		})
	}
	return resp, nil
}

func splitTypeHeader(v []byte) (string, []byte) {
	if i := indexByte(v, '\n'); i > 6 && string(v[:6]) == "type: " {
		return string(v[6:i]), v[i+1:]
	}
	return "unknown", v
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func observedState(e *store.Entry) string {
	if e == nil {
		return ""
	}
	return string(e.Value)
}

func healthOfStatus(e *store.Entry) pb.Health {
	if e == nil {
		return pb.Health_HEALTH_UNKNOWN
	}
	v := string(e.Value)
	switch {
	case strings.Contains(v, "health=healthy"):
		return pb.Health_HEALTH_HEALTHY
	case strings.Contains(v, "health=degraded"):
		return pb.Health_HEALTH_DEGRADED
	case strings.Contains(v, "health=unhealthy"):
		return pb.Health_HEALTH_UNHEALTHY
	}
	return pb.Health_HEALTH_UNKNOWN
}

// GetResource returns a single desired-state resource.
func (s *Server) GetResource(ctx context.Context, req *pb.GetResourceRequest) (*pb.Resource, error) {
	if req.Id == "" {
		return nil, errors.New(errors.KindInvalid, "GetResource", "id required")
	}
	e, err := s.store.Get(ctx, s.agent.Reconciler().DesiredPrefix()+store.Key(req.Id))
	if err != nil {
		return nil, mapErr("GetResource", err)
	}
	typ, spec := splitTypeHeader(e.Value)
	st, _ := s.store.Get(ctx, s.agent.Reconciler().StatusPrefix()+store.Key(req.Id))
	return &pb.Resource{
		Id:              req.Id,
		Type:            typ,
		DesiredState:    string(spec),
		ObservedState:   observedState(st),
		Health:          healthOfStatus(st),
		UpdatedAtUnixNs: e.UpdatedAt,
	}, nil
}

// ApplyResources applies a YAML desired-state document.
func (s *Server) ApplyResources(ctx context.Context, req *pb.ApplyResourcesRequest) (*pb.ApplyResourcesResponse, error) {
	if len(req.Spec) == 0 {
		return nil, errors.New(errors.KindInvalid, "ApplyResources", "empty spec")
	}
	applied, failed, errs := s.agent.ApplySpec(ctx, req.Spec)
	return &pb.ApplyResourcesResponse{
		Applied: int64(applied),
		Failed:  int64(failed),
		Errors:  errs,
	}, nil
}

// DeleteResource removes a resource: desired state + managed object.
func (s *Server) DeleteResource(ctx context.Context, req *pb.DeleteResourceRequest) (*pb.DeleteResourceResponse, error) {
	if req.Id == "" {
		return nil, errors.New(errors.KindInvalid, "DeleteResource", "id required")
	}
	deleted, err := s.agent.DeleteResource(ctx, req.Id)
	if err != nil {
		return nil, mapErr("DeleteResource", err)
	}
	return &pb.DeleteResourceResponse{Deleted: deleted}, nil
}

// Reconcile triggers a tick and streams progress events.
func (s *Server) Reconcile(req *pb.ReconcileRequest, stream pb.NodeService_ReconcileServer) error {
	s.send(stream, "starting", "reconcile requested", 0)
	if req.DryRun {
		s.agent.Reconciler().SetDryRun(true)
		defer s.agent.Reconciler().SetDryRun(false)
	}
	before := s.agent.Reconciler().Metrics()
	if err := s.agent.Reconciler().Tick(stream.Context()); err != nil {
		return mapErr("Reconcile", err)
	}
	after := s.agent.Reconciler().Metrics()
	msg := fmt.Sprintf("%d resources, %d changes, %d failures (took %.0f ms)",
		after.ResourcesTotal, after.ChangesApplied-before.ChangesApplied,
		after.Failures-before.Failures, after.LastTickTookMs)
	s.send(stream, "complete", msg, 100)
	return nil
}

func (s *Server) send(stream pb.NodeService_ReconcileServer, phase, msg string, pct float64) {
	_ = stream.Send(&pb.ReconcileEvent{Phase: phase, Message: msg, ProgressPct: pct})
}

// StreamEvents streams store watch events under a prefix.
func (s *Server) StreamEvents(req *pb.StreamEventsRequest, stream pb.NodeService_StreamEventsServer) error {
	ctx := stream.Context()
	prefix := store.Key(req.Prefix)
	if prefix == "" || prefix == "/" {
		prefix = s.agent.Reconciler().DesiredPrefix()
	}
	cur, err := s.store.Revision(ctx)
	if err != nil {
		return mapErr("StreamEvents", err)
	}
	ch, err := s.store.Watch(ctx, prefix, cur)
	if err != nil {
		return mapErr("StreamEvents", err)
	}
	s.logger.Info("events stream open", "prefix", string(prefix))
	for ev := range ch {
		typ := "put"
		if ev.Type == store.EventDelete {
			typ = "delete"
		}
		payload := ""
		if ev.Entry != nil {
			payload = string(ev.Entry.Value)
		}
		if err := stream.Send(&pb.Event{
			Type:            typ,
			Payload:         payload,
			TimestampUnixNs: time.Now().UnixNano(),
		}); err != nil {
			return err
		}
	}
	return nil
}

// GetHealth returns the current health report.
func (s *Server) GetHealth(ctx context.Context, req *pb.GetHealthRequest) (*pb.HealthReport, error) {
	rep := s.agent.Health(ctx)
	if rep == nil {
		return nil, errors.New(errors.KindUnavailable, "GetHealth", "health not collected yet")
	}
	out := &pb.HealthReport{}
	switch rep.Overall {
	case health.Healthy:
		out.Overall = pb.HealthReport_OVERALL_HEALTHY
	case health.Degraded:
		out.Overall = pb.HealthReport_OVERALL_DEGRADED
	case health.Unhealthy:
		out.Overall = pb.HealthReport_OVERALL_UNHEALTHY
	default:
		out.Overall = pb.HealthReport_OVERALL_UNSPECIFIED
	}
	for _, c := range rep.Checks {
		out.Checks = append(out.Checks, &pb.CheckResult{
			Name:    c.Name,
			Status:  pbHealth(c.Status),
			Message: c.Message,
			Details: c.Details,
			TookMs:  c.Took.Milliseconds(),
		})
	}
	return out, nil
}

func pbHealth(h health.Health) pb.Health {
	switch h {
	case health.Healthy:
		return pb.Health_HEALTH_HEALTHY
	case health.Degraded:
		return pb.Health_HEALTH_DEGRADED
	case health.Unhealthy:
		return pb.Health_HEALTH_UNHEALTHY
	default:
		return pb.Health_HEALTH_UNKNOWN
	}
}

// Shutdown requests a graceful agent shutdown.
func (s *Server) Shutdown(ctx context.Context, req *pb.ShutdownRequest) (*pb.ShutdownResponse, error) {
	s.agent.Shutdown("api request")
	return &pb.ShutdownResponse{Success: true, Message: "shutdown initiated"}, nil
}

// --- Generations (§4.7) ---

func genInfo(g *generation.Generation, keys []string) *pb.GenerationInfo {
	info := &pb.GenerationInfo{
		Number:          g.Number,
		Revision:        uint64(g.Revision),
		CreatedAtUnixNs: g.CreatedAt.UnixNano(),
		CreatedBy:       g.CreatedBy,
		Description:     g.Description,
		Parent:          g.Parent,
		Hash:            g.Hash,
		Keys:            keys,
	}
	return info
}

// ListGenerations returns the retained generation history, oldest first.
func (s *Server) ListGenerations(ctx context.Context, req *pb.ListGenerationsRequest) (*pb.ListGenerationsResponse, error) {
	gens, err := generation.List(ctx, s.store)
	if err != nil {
		return nil, mapErr("ListGenerations", err)
	}
	out := &pb.ListGenerationsResponse{}
	for i := range gens {
		out.Generations = append(out.Generations, genInfo(&gens[i], nil))
	}
	return out, nil
}

// GetGeneration returns one generation's metadata and (optionally) its keys.
func (s *Server) GetGeneration(ctx context.Context, req *pb.GetGenerationRequest) (*pb.GenerationInfo, error) {
	g, err := generation.Get(ctx, s.store, req.Number)
	if err != nil {
		return nil, mapErr("GetGeneration", err)
	}
	var keys []string
	if req.IncludeKeys {
		content, err := generation.Content(ctx, s.store, req.Number)
		if err != nil {
			return nil, mapErr("GetGeneration", err)
		}
		for k := range content {
			keys = append(keys, string(k))
		}
		sort.Strings(keys)
	}
	return genInfo(g, keys), nil
}

// DiffGenerations compares two snapshots.
func (s *Server) DiffGenerations(ctx context.Context, req *pb.DiffGenerationsRequest) (*pb.DiffGenerationsResponse, error) {
	d, err := generation.DiffGenerations(ctx, s.store, req.A, req.B)
	if err != nil {
		return nil, mapErr("DiffGenerations", err)
	}
	return &pb.DiffGenerationsResponse{Added: d.Added, Removed: d.Removed, Changed: d.Changed}, nil
}

// RollbackGeneration restores a prior generation's desired state as a new
// generation (append-only — see the generation package comment).
func (s *Server) RollbackGeneration(ctx context.Context, req *pb.RollbackGenerationRequest) (*pb.RollbackGenerationResponse, error) {
	n, err := generation.Rollback(ctx, s.store, req.Target, "user", "")
	if err != nil {
		return nil, mapErr("RollbackGeneration", err)
	}
	return &pb.RollbackGenerationResponse{NewGeneration: n}, nil
}

// mapErr converts typed errors to gRPC codes; unknown errors become
// Internal with the original message.
func mapErr(op string, err error) error {
	switch errors.KindOf(err) {
	case errors.KindNotFound:
		return status.Error(codes.NotFound, op+": "+err.Error())
	case errors.KindConflict:
		return status.Error(codes.Aborted, op+": "+err.Error())
	case errors.KindInvalid:
		return status.Error(codes.InvalidArgument, op+": "+err.Error())
	case errors.KindUnavailable:
		return status.Error(codes.Unavailable, op+": "+err.Error())
	case errors.KindTimeout:
		return status.Error(codes.DeadlineExceeded, op+": "+err.Error())
	case errors.KindPermission:
		return status.Error(codes.PermissionDenied, op+": "+err.Error())
	default:
		return status.Error(codes.Internal, op+": "+err.Error())
	}
}

// ---- kv + cluster status (§5, used by VM tests and scripting) ----

// PutKeyValue writes k→v through the store. In cluster mode this is a
// Raft write: it fails fast with Unavailable when degraded (§4.10.3).
func (s *Server) PutKeyValue(ctx context.Context, req *pb.PutKeyValueRequest) (*pb.PutKeyValueResponse, error) {
	if req.GetKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "PutKeyValue: empty key")
	}
	rev, err := s.store.Put(ctx, store.Key(req.GetKey()), req.GetValue())
	if err != nil {
		return nil, mapErr("PutKeyValue", err)
	}
	return &pb.PutKeyValueResponse{Revision: int64(rev)}, nil
}

// GetKeyValue reads a key: linearizable by default (followers forward
// to the leader), or the local FSM copy when stale is set — the only
// read path a degraded node still serves (§4.10.3).
func (s *Server) GetKeyValue(ctx context.Context, req *pb.GetKeyValueRequest) (*pb.GetKeyValueResponse, error) {
	c := ctx
	if req.GetStale() {
		c = store.WithStale(ctx)
	}
	e, err := s.store.Get(c, store.Key(req.GetKey()))
	switch {
	case errors.Is(err, errors.KindNotFound):
		return &pb.GetKeyValueResponse{Found: false}, nil
	case err != nil:
		return nil, mapErr("GetKeyValue", err)
	}
	return &pb.GetKeyValueResponse{Found: true, Value: e.Value, Revision: int64(e.Revision)}, nil
}

// DeleteKeyValue removes a key through the store (Raft write in
// cluster mode).
func (s *Server) DeleteKeyValue(ctx context.Context, req *pb.DeleteKeyValueRequest) (*pb.DeleteKeyValueResponse, error) {
	if err := s.store.Delete(ctx, store.Key(req.GetKey()), 0); err != nil {
		return nil, mapErr("DeleteKeyValue", err)
	}
	return &pb.DeleteKeyValueResponse{}, nil
}

// GetClusterStatus builds the §5 cluster report from the local store.
// Only meaningful in cluster mode (the bolt store of a single node has
// no cluster records — we still render a minimal report).
func (s *Server) GetClusterStatus(ctx context.Context, req *pb.GetClusterStatusRequest) (*pb.GetClusterStatusResponse, error) {
	rs, ok := s.store.(*raftstore.Store)
	if !ok {
		return nil, status.Error(codes.FailedPrecondition, "GetClusterStatus: not a cluster-mode agent")
	}
	rp, err := control.Status(ctx, rs)
	if err != nil {
		// A degraded follower can't linearize: status is advisory, so
		// serve the last-known FSM copy (§4.10.3: stale reads keep
		// working) rather than failing when it matters most.
		rp, err = control.Status(store.WithStale(ctx), rs)
	}
	if err != nil {
		return nil, mapErr("GetClusterStatus", err)
	}
	b, err := json.Marshal(rp)
	if err != nil {
		return nil, status.Error(codes.Internal, "GetClusterStatus: "+err.Error())
	}
	return &pb.GetClusterStatusResponse{ReportJson: b}, nil
}
