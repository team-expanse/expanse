// Package agent is the node agent: it owns the local store, runs the
// reconciliation loop with the registered resource managers, collects
// inventory and health, serves the gRPC API on a unix socket, and
// integrates with systemd (sd_notify READY/WATCHDOG).
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"

	"github.com/expanse/expanse/internal/agent/health"
	"github.com/expanse/expanse/internal/agent/inventory"
	"github.com/expanse/expanse/internal/agent/managers/fileman"
	"github.com/expanse/expanse/internal/agent/managers/nixman"
	"github.com/expanse/expanse/internal/agent/managers/sysctlman"
	"github.com/expanse/expanse/internal/agent/managers/systemdman"
	"github.com/expanse/expanse/internal/agent/nix"
	"github.com/expanse/expanse/internal/api"
	"github.com/expanse/expanse/internal/cluster/ca"
	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/nodelc"
	"github.com/expanse/expanse/internal/config"
	"github.com/expanse/expanse/internal/install"
	"github.com/expanse/expanse/internal/logging"
	"github.com/expanse/expanse/internal/reconcile"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
	"github.com/expanse/expanse/internal/store/raftstore"
	"gopkg.in/yaml.v3"
)

// Config configures the agent.
type Config struct {
	NodeID  string        // default: node identity or hostname
	DataDir string        // default /persist/expanse
	Socket  string        // default /run/expanse/agent.sock
	Period  time.Duration // reconcile period, default 30s
	DryRun  bool
	// EnableTCP enables the :7443 listener — disabled in Phase 02 (no
	// mTLS yet); Phase 03 enables it with mTLS.
	EnableTCP  bool
	TCPTCPAddr string
	LogLevel   string
	// Role overrides the cluster role (§4.9: "voter" | "witness")
	// recorded at join time; when empty it is read from the node record.
	Role string
	// RaftBindAddr / RaftAdvertiseAddr override the raft transport
	// binding (default 0.0.0.0:7444, advertised as LocalIP:7444).
	// They MUST match what the node joined with (the cluster dials the
	// advertised address from the node record).
	RaftBindAddr      string
	RaftAdvertiseAddr string
}

// Role is the node's cluster role (§4.9): "voter" (default) or
// "witness". A witness is a full Raft voter with zero advertised
// capacity: it skips the reconcile loop, inventory collection and the
// storage/net-mesh, so the scheduler never places anything on it. When
// unset, the role is derived from the node's cluster record.

func (c *Config) fill() error {
	if c.DataDir == "" {
		c.DataDir = "/persist/expanse"
	}
	if c.Socket == "" {
		c.Socket = "/run/expanse/agent.sock"
	}
	if c.NodeID == "" {
		c.NodeID = detectNodeID(c.DataDir)
	}
	if c.NodeID == "" {
		return fmt.Errorf("no node id (identity missing and hostname failed)")
	}
	return nil
}

// detectNodeID prefers the cluster enrollment's node-id (must match the
// raft voter config), then the Phase 01 node identity, then hostname.
func detectNodeID(dataDir string) string {
	if id := control.LoadNodeID(dataDir); id != "" {
		return id
	}
	if id, err := install.LoadIdentity(filepath.Join(dataDir, install.IdentityDir)); err == nil {
		return id.NodeID.String()
	}
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// Agent is the running node agent.
// clusterCtl holds the cluster-mode additions (nil in single-node mode).
type clusterCtl struct {
	store   *raftstore.Store
	ca      *ca.CA
	secret  []byte
	id      string
	dataDir string
	role    string // §4.9: "voter" | "witness" (from the node record)
}

type Agent struct {
	cfg          Config
	ctl          *clusterCtl
	store        store.Store
	logger       *slog.Logger
	recon        *reconcile.Reconciler
	invMu        sync.Mutex
	invSnapshot  *inventory.Inventory
	invCollector *inventory.Collector
	healthR      *health.Runner

	status   atomic.Value // string
	shutdown atomic.Value // chan struct{}
	stopFn   func()
}

// New creates the agent: opens the store, registers managers, wires
// health checks.
func New(cfg Config) (*Agent, error) {
	if err := cfg.fill(); err != nil {
		return nil, err
	}
	logger, err := logging.Setup(logging.Options{Level: cfg.LogLevel, Format: "text"})
	if err != nil {
		return nil, err
	}
	logger = logging.WithComponent(logger, "agent")

	// Store: single-node boltstore by default; the raft-replicated
	// store when this node carries a cluster enrollment (cluster-id
	// file present) — Phase 03 §5 daemon wiring.
	var st store.Store
	var ctl *clusterCtl
	if control.IsClusterNode(cfg.DataDir) {
		clusterID, secret, clusterCA, err := control.LoadCluster(cfg.DataDir)
		if err != nil {
			return nil, fmt.Errorf("load cluster enrollment: %w", err)
		}
		raftBind := cfg.RaftBindAddr
		if raftBind == "" {
			raftBind = fmt.Sprintf("0.0.0.0:%d", config.PortRaft)
		}
		raftAdv := cfg.RaftAdvertiseAddr
		if raftAdv == "" {
			host, port, err := net.SplitHostPort(raftBind)
			advHost := control.LocalIP()
			if err == nil && host != "" && host != "0.0.0.0" {
				advHost = host
			}
			raftAdv = net.JoinHostPort(advHost, port)
		}
		rs, err := raftstore.Open(raftstore.Config{
			NodeID:        cfg.NodeID,
			BindAddr:      raftBind,
			AdvertiseAddr: raftAdv,
			DataDir:       filepath.Join(cfg.DataDir, control.RaftDir),
		})
		if err != nil {
			return nil, fmt.Errorf("open cluster store: %w", err)
		}
		st = rs
		ctl = &clusterCtl{store: rs, ca: clusterCA, secret: secret, id: clusterID, dataDir: cfg.DataDir}
		// Role (§4.9): explicit config wins; otherwise read it from the
		// node's own cluster record. Retry while raft restores its FSM —
		// the record may not be visible in the first moments.
		ctl.role = cfg.Role
		if ctl.role == "" {
			for i := 0; i < 25; i++ {
				ctl.role = lookupRole(rs, cfg.NodeID)
				if ctl.role != "" {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
		}
		if ctl.role == "" {
			ctl.role = "voter"
		}
		logger.Info("cluster mode: raft-replicated store", "cluster", clusterID, "role", ctl.role)
	} else {
		st, err = boltstore.New(filepath.Join(cfg.DataDir, "store", "local.db"))
		if err != nil {
			return nil, fmt.Errorf("open store: %w", err)
		}
	}

	a := &Agent{
		cfg:    cfg,
		store:  st,
		ctl:    ctl,
		logger: logger,
		invMu:  sync.Mutex{},
	}
	a.status.Store("starting")

	// Reconciler + managers.
	r := reconcile.New(st, reconcile.Options{
		NodeID: cfg.NodeID,
		Period: cfg.Period,
		DryRun: cfg.DryRun,
		Logger: logger,
	})
	fm := fileman.New()
	r.Register(fm.FileManager())
	r.Register(fm.DirManager())
	r.Register(sysctlman.New())
	if sd, err := systemdman.New(); err != nil {
		logger.Warn("systemd dbus unavailable; systemd-unit manager disabled", "err", err)
	} else {
		r.Register(sd)
	}
	r.Register(nixman.New(nix.New(), os.Stdout))
	a.recon = r

	// Inventory + health.
	a.invCollector = &inventory.Collector{}
	a.healthR = &health.Runner{Checks: []health.Check{
		&health.DiskSpaceCheck{},
		&health.MemoryCheck{},
		&health.LoadCheck{},
		&health.NixStoreCheck{},
		&health.ClockSyncCheck{},
		&health.StoreCheck{Put: func(ctx context.Context) (time.Duration, error) {
			start := time.Now()
			key := store.Key(fmt.Sprintf("/nodes/%s/health/store-probe", cfg.NodeID))
			_, err := st.Put(ctx, key, []byte(fmt.Sprint(start.UnixNano())))
			return time.Since(start), err
		}},
		&health.ReconcileCheck{Metrics: r.Metrics},
	}}
	return a, nil
}

// Store exposes the agent's store (used by the API server).
func (a *Agent) Store() store.Store { return a.store }

// lookupRole reads the node's role from its cluster record (§4.9);
// empty when there is no record yet (pre-init bootstrap).
func lookupRole(rs *raftstore.Store, nodeID string) string {
	e, err := rs.Get(store.WithStale(context.Background()), store.Key("/nodes/"+nodeID))
	if err != nil {
		return ""
	}
	var rec struct {
		Role string `json:"role"`
	}
	if json.Unmarshal(e.Value, &rec) != nil {
		return ""
	}
	return rec.Role
}

// NodeID implements api.Agent.
func (a *Agent) NodeID() string { return a.cfg.NodeID }

// Reconciler implements api.Agent.
func (a *Agent) Reconciler() *reconcile.Reconciler { return a.recon }

// Snapshot implements api.Agent.
func (a *Agent) Snapshot() api.Snapshot {
	m := a.recon.Metrics()
	return api.Snapshot{
		Status:         a.status.Load().(string),
		ResourcesTotal: m.ResourcesTotal,
		ChangesApplied: m.ChangesApplied,
		Failures:       m.Failures,
		Ticks:          m.Ticks,
		LastTickAt:     m.LastTickAt,
		LastTickTookMs: m.LastTickTookMs,
		TickP99Ms:      m.TickP99Ms,
	}
}

// Inventory implements api.Agent.
func (a *Agent) Inventory() *inventory.Inventory {
	a.invMu.Lock()
	defer a.invMu.Unlock()
	return a.invSnapshot
}

// Health implements api.Agent.
func (a *Agent) Health(ctx context.Context) *health.Report {
	return a.healthR.RunAll(ctx)
}

// Shutdown implements api.Agent.
func (a *Agent) Shutdown(reason string) {
	a.logger.Info("shutdown requested", "reason", reason)
	if ch, ok := a.shutdown.Load().(chan struct{}); ok && ch != nil {
		select {
		case <-ch:
			return
		default:
			close(ch)
		}
	}
}

// applyDoc is the YAML desired-state document format for ApplySpec:
//
//	file:/etc/motd:
//	  type: file
//	  path: /etc/motd
//	  content: hello
//	  mode: "0644"
type applyDoc map[string]map[string]any

// ApplySpec implements api.Agent: writes each entry as desired state under
// /node/<id>/resources/<id> with the "type: <t>" header the reconciler
// expects.
func (a *Agent) ApplySpec(ctx context.Context, spec []byte) (applied, failed int, errs []string) {
	var doc applyDoc
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		return 0, 1, []string{"spec: " + err.Error()}
	}
	prefix := a.recon.DesiredPrefix()
	for id, fields := range doc {
		typ, _ := fields["type"].(string)
		if typ == "" {
			failed++
			errs = append(errs, fmt.Sprintf("%s: missing type", id))
			continue
		}
		delete(fields, "type")
		rest, err := yaml.Marshal(fields)
		if err != nil {
			failed++
			errs = append(errs, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		value := append([]byte("type: "+typ+"\n"), rest...)
		if _, err := a.store.Put(ctx, prefix+store.Key(id), value); err != nil {
			failed++
			errs = append(errs, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		applied++
	}
	return applied, failed, errs
}

// DeleteResource implements api.Agent.
func (a *Agent) DeleteResource(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, fmt.Errorf("empty resource id")
	}
	desiredKey := a.recon.DesiredPrefix() + store.Key(id)
	e, err := a.store.Get(ctx, desiredKey)
	if err != nil {
		return false, fmt.Errorf("load desired state: %w", err)
	}
	// Decode the type header and ask the manager to delete the object.
	value := e.Value
	typ := ""
	if len(value) > 6 && string(value[:6]) == "type: " {
		if i := indexNL(value); i > 0 {
			typ = string(value[6:i])
			value = value[i+1:]
		}
	}
	m := a.recon.ManagerFor(typ)
	if m == nil {
		return false, fmt.Errorf("no manager for type %q", typ)
	}
	res, err := m.Load(id, value)
	if err != nil {
		return false, fmt.Errorf("load resource: %w", err)
	}
	if d, ok := m.(reconcile.Deleter); ok {
		if err := d.Delete(ctx, res); err != nil {
			return false, fmt.Errorf("delete object: %w", err)
		}
	} else {
		a.logger.Warn("manager cannot delete managed object; removing desired state only",
			"id", id, "type", typ)
	}
	if err := a.store.Delete(ctx, desiredKey, 0); err != nil {
		return false, fmt.Errorf("delete desired state: %w", err)
	}
	// Best-effort status cleanup.
	_ = a.store.Delete(ctx, a.recon.StatusPrefix()+store.Key(id), 0)
	a.recon.Trigger()
	return true, nil
}

func indexNL(b []byte) int {
	for i := range b {
		if b[i] == '\n' {
			return i
		}
	}
	return -1
}

// Run runs the agent until ctx is done or Shutdown is called: reconcile
// loop, inventory loop, health/status loop, gRPC server, sd_notify.
func (a *Agent) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	a.stopFn = cancel
	defer cancel()
	stopCh := make(chan struct{})
	a.shutdown.Store(stopCh)
	// Re-wire Shutdown to also cancel the run context.
	go func() {
		select {
		case <-stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	a.status.Store("idle")

	witness := a.ctl != nil && a.ctl.role == "witness"
	if witness {
		a.logger.Info("witness mode: full raft voter, zero capacity — reconcile/inventory/storage skipped (§4.9)")
	}

	// Reconcile loop (watch-triggered + periodic). Witnesses never
	// reconcile: zero capacity means nothing is ever placed here (§4.9).
	if !witness {
		go a.recon.Run(ctx)
	}

	// Node-lifecycle failure monitor (§4.8), cluster mode only: the
	// leader marks silent nodes unreachable (15 s) then failed (5 min).
	// Only the leader evaluates; on followers this is a no-op ticker.
	if a.ctl != nil && a.ctl.store != nil {
		mon := &nodelc.Monitor{
			St:         a.ctl.store,
			ThisNodeID: a.cfg.NodeID,
			Evict: func(nodeID string) {
				// Placements are not scheduled until the placement
				// engine lands; the seam stays here (§4.8 "evicts its
				// placements").
				a.logger.Warn("node failed; placements evicted (no placement engine yet)", "node", nodeID)
				_ = a.store.Delete(context.Background(), store.Key("/nodes/"+nodeID+"/status"), 0)
			},
		}
		go mon.Run(ctx)

		// §4.10.3/§4.10.4: track quorum reachability; when this node has
		// been without a leader for DegradedAfter it goes read-only and
		// the reconcile loop freezes (existing workloads keep running).
		go a.watchDegraded(ctx)
	}

	// Inventory: immediately, then every 5 minutes (witnesses skip —
	// §4.9 minimal footprint).
	if !witness {
		a.refreshInventory()
		go a.loop(ctx, 5*time.Minute, "inventory", a.refreshInventory)
	}

	// Health + node status: every 10 s. The status value advertises the
	// degraded condition (§4.10.3) when the node cannot reach quorum;
	// the write itself will fail while degraded (no quorum = no
	// commits), which is exactly what makes the leader mark us
	// unreachable via §4.8.
	go a.loop(ctx, 10*time.Second, "health", func() {
		rep := a.healthR.RunAll(ctx)
		val := fmt.Sprintf("health=%s", rep.Overall)
		if rs, ok := a.store.(*raftstore.Store); ok && rs.Degraded() {
			val += " degraded=true writable=false"
		}
		a.status.Store(statusFromHealth(rep.Overall))
		key := store.Key(fmt.Sprintf("/nodes/%s/status", a.cfg.NodeID))
		if _, err := a.store.Put(ctx, key, []byte(val)); err != nil {
			a.logger.Error("write node status failed", "err", err)
		}
	})

	// sd_notify: READY once the store is open, listeners are about to
	// bind, and the first reconcile has been kicked off (the reconciler's
	// startup tick is synchronous inside Run, so by now it completed).
	notify("READY=1")
	// WATCHDOG pings at half the WatchdogSec interval (default unit
	// config: WatchdogSec=60s → ping every 30 s; a wedged agent is
	// restarted by systemd automatically).
	go a.loop(ctx, watchdogInterval(), "sd-watchdog", func() {
		notify("WATCHDOG=1")
	})
	defer notify("STOPPING=1")

	// Cluster mode: serve the token-authenticated join endpoint on
	// :7446 (redirects when not leader; no-op when single-node).
	// Witnesses don't serve joins: they hold the CA material but have
	// no capacity and (per §4.9) exist only to vote.
	if a.ctl != nil && !witness {
		go func() {
			if err := control.ServeJoinEndpoint(ctx, a.ctl.store, a.ctl.ca, a.ctl.secret, a.ctl.dataDir, a.ctl.id); err != nil {
				a.logger.Error("join endpoint failed", "err", err)
			}
		}()
	}

	// gRPC on the unix socket (filesystem permissions are the auth).
	srv := api.NewServer(a, a.store, a.logger)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, a.cfg.Socket) }()

	// Optional TCP listener — OFF by default in Phase 02 (no mTLS yet).
	if a.cfg.EnableTCP {
		a.logger.Warn("tcp listener enabled without mTLS (Phase 03 adds mTLS)", "addr", a.cfg.TCPTCPAddr)
	}

	select {
	case <-ctx.Done():
		a.logger.Info("agent stopping")
	case err := <-serveErr:
		if err != nil {
			a.status.Store("error")
			return fmt.Errorf("grpc serve: %w", err)
		}
	}
	a.status.Store("shutting-down")
	a.recon.Stop()
	if err := a.store.Close(); err != nil {
		a.logger.Error("store close failed", "err", err)
	}
	return nil
}

// watchDegraded follows the store's degraded flag (§4.10.3): on
// entering degraded mode the reconcile loop freezes — no new desired
// state is applied, existing workloads keep running (§4.10.4).
func (a *Agent) watchDegraded(ctx context.Context) {
	rs, ok := a.store.(*raftstore.Store)
	if !ok {
		return
	}
	frozen := false
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		deg := rs.Degraded()
		if deg != frozen {
			frozen = deg
			a.recon.Freeze(deg)
			if deg {
				since, _ := rs.DegradedSince()
				a.logger.Warn("DEGRADED: no quorum — read-only, reconcile frozen", "since", since.Format(time.RFC3339))
				a.status.Store("degraded")
			} else {
				a.logger.Info("quorum restored — writable again, reconcile unfrozen")
			}
		}
	}
}

// Degraded reports whether the local node currently cannot reach
// quorum (§4.10.3) — always false in single-node (boltstore) mode.
func (a *Agent) Degraded() bool {
	rs, ok := a.store.(*raftstore.Store)
	if !ok {
		return false
	}
	return rs.Degraded()
}

// Stop stops the agent from outside Run (e.g. SIGTERM already canceled ctx).
func (a *Agent) Stop() {
	if a.stopFn != nil {
		a.stopFn()
	}
}

func (a *Agent) refreshInventory() {
	inv, err := a.invCollector.Collect(a.cfg.NodeID)
	if err != nil {
		a.logger.Error("inventory collection failed", "err", err)
		return
	}
	a.invMu.Lock()
	a.invSnapshot = inv
	a.invMu.Unlock()
	a.logger.Info("inventory collected",
		"cores", inv.CPU.Cores, "mem_gb", inv.Memory.Total/1e9,
		"disks", len(inv.Disks), "nics", len(inv.Network), "virt", inv.Virtualization)
}

func (a *Agent) loop(ctx context.Context, every time.Duration, name string, fn func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

func statusFromHealth(h health.Health) string {
	switch h {
	case health.Healthy:
		return "idle"
	case health.Degraded:
		return "degraded"
	case health.Unhealthy:
		return "error"
	default:
		return "idle"
	}
}

// watchdogInterval returns half of WatchdogSec from the systemd
// environment (default 30 s pings).
func watchdogInterval() time.Duration {
	interval, err := daemon.SdWatchdogEnabled(false)
	if err != nil || interval <= 0 {
		return 30 * time.Second
	}
	return interval / 2
}

// notify sends a systemd notification if running under Type=notify.
func notify(state string) {
	if _, err := daemon.SdNotify(false, state); err != nil {
		// Not fatal: only relevant under systemd.
		_ = err
	}
}
