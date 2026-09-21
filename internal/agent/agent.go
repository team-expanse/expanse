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
	"net/netip"
	"os"
	"path/filepath"
	"strings"
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
	"github.com/expanse/expanse/internal/cluster/lease"
	"github.com/expanse/expanse/internal/network/vip"
	"github.com/expanse/expanse/internal/proxy"

	volctlc "github.com/expanse/expanse/internal/storage/controller"
	expmount "github.com/expanse/expanse/internal/storage/mount"
	pb "github.com/expanse/expanse/proto"

	"github.com/expanse/expanse/internal/api"
	"github.com/expanse/expanse/internal/blocks/catalog"
	"github.com/expanse/expanse/internal/blocks/controller"
	"github.com/expanse/expanse/internal/blocks/runtime/systemd"
	"github.com/expanse/expanse/internal/blocks/service"
	"github.com/expanse/expanse/internal/blocks/validate"
	"github.com/expanse/expanse/internal/blocks/wire"
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
	"google.golang.org/grpc/credentials"
	"gopkg.in/yaml.v3"
)

// Config configures the agent.
type Config struct {
	NodeID  string        // default: node identity or hostname
	DataDir string        // default /persist/expanse
	Socket  string        // default /run/expanse/agent.sock
	Period  time.Duration // reconcile period, default 30s
	// ControllerPeriod is the block placement controller's backstop pass
	// interval (§4.3 retry timer, default 30s). Shorter in tests.
	ControllerPeriod time.Duration
	DryRun           bool
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
	// BlocksCatalog is the shipped block-type directory (nix/blocks
	// layout: <category>/<name>/block.yaml...). When empty, the block
	// API is not served (blocks disabled on this node).
	BlocksCatalog string
	// BlocksFlakeRef is the flake reference holding block closures
	// (attr per type: <category>-<name>, e.g. "util-echo"). Empty =
	// block replicas are not realized on this node (API-only member).
	BlocksFlakeRef string
	// ExternalVIPPool is the cluster.network.externalVIPPool value
	// (§4.2): e.g. "192.168.1.100-192.168.1.120". Empty = no external
	// VIPs; VIP-exposed blocks allocate from the internal range.
	ExternalVIPPool string
	// ExternalInterface is the physical interface to announce external
	// VIPs on ("" / "auto" = the default route's interface).
	ExternalInterface string
	// DNSUpstreams overrides the DNS forwarders (T17): comma-separated
	// "ip:port" list. Empty = parsed from /etc/resolv.conf.
	DNSUpstreams string
	// StorageVG is the LVM volume group the node's volume replicas live in.
	// Empty = volume storage disabled on this node (e.g. witnesses).
	StorageVG string
	// StoragePool is the thin pool inside StorageVG; empty makes thick volumes.
	StoragePool string
	// DRBDConfigDir receives one DRBD resource file per volume (default /etc/drbd.d).
	DRBDConfigDir string
	// Firewall enables the §4.5 nftables ruleset (T19/T20): static
	// skeleton at start, store-driven dynamic sets after. Off by
	// default — deployments that manage the host firewall themselves
	// (or run inside a dedicated network namespace) opt out.
	Firewall bool
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
	if c.DRBDConfigDir == "" {
		c.DRBDConfigDir = defaultDRBDConfigDir
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
	role    string                   // §4.9: "voter" | "witness" (from the node record)
	fwd     *raftstore.GRPCForwarder // node↔node write/read forwarding
}

type Agent struct {
	cfg          Config
	ctl          *clusterCtl
	store        store.Store
	logger       *slog.Logger
	recon        *reconcile.Reconciler
	blocks       pb.BlockServiceServer
	blockCatalog pb.CatalogServiceServer
	blockCtl     *controller.Controller
	nodeLease    *lease.Held
	volnode      volumeRunner
	volctl       *volctlc.Controller
	blockBridge  *wire.Bridge
	invMu        sync.Mutex
	invSnapshot  *inventory.Inventory
	invCollector *inventory.Collector
	healthR      *health.Runner

	// §4.6 network policy: cached compiled-ruleset key so unchanged
	// passes skip kernel traffic entirely.
	fwPolKey string

	// VIP management (§4.2, internal/agent/vip.go).
	vipMu     sync.Mutex
	extPool   []netip.Prefix
	extIface  string
	vipCands  map[string][]vip.Candidate
	holders   map[string]*holderRun
	vipLeases *lease.Manager

	// LB wiring (§4.3, internal/agent/lb.go).
	lbPool   *proxy.Pool
	nodeMu   sync.Mutex
	nodeAddr map[string]nodeAddr

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
		raftAdv := cfg.RaftAdvertiseAddr
		if raftBind == "" || raftAdv == "" {
			// Prefer the addresses recorded at init/join (§4.4 layout):
			// the CLI already advertised them to peers.
			rb, ra := control.LoadRaftAddr(cfg.DataDir)
			if raftBind == "" {
				raftBind = rb
			}
			if raftAdv == "" {
				raftAdv = ra
			}
		}
		if raftBind == "" {
			raftBind = fmt.Sprintf("0.0.0.0:%d", config.PortRaft)
		}
		if raftAdv == "" {
			host, port, err := net.SplitHostPort(raftBind)
			advHost := control.LocalIP()
			if err == nil && host != "" && host != "0.0.0.0" {
				advHost = host
			}
			raftAdv = net.JoinHostPort(advHost, port)
		}
		// Persist the addresses this boot actually settled on —
		// regardless of whether they came from flags, the file, or the
		// LocalIP guess above. mesh.go's lanIP() depends on this file to
		// know the node's real LAN address; a node started with a static
		// --raft-advertise (every declaratively-configured cluster
		// member) would otherwise never write it, forcing lanIP() to
		// fall back to interface-enumeration guessing forever, on every
		// tick, across every restart.
		if err := control.SaveRaftAddr(cfg.DataDir, raftBind, raftAdv); err != nil {
			logger.Warn("save raft addr failed", "err", err)
		}
		rs, err := raftstore.Open(raftstore.Config{
			NodeID:        cfg.NodeID,
			BindAddr:      raftBind,
			AdvertiseAddr: raftAdv,
			DataDir:       filepath.Join(cfg.DataDir, control.RaftDir),
			// Surface hashicorp raft's internal narrative (elections,
			// step-downs) in the agent log — invaluable when a
			// cluster misbehaves.
			LogOutput: raftLogWriter{log: logger},
		})
		if err != nil {
			return nil, fmt.Errorf("open cluster store: %w", err)
		}
		st = rs
		ctl = &clusterCtl{store: rs, ca: clusterCA, secret: secret, id: clusterID, dataDir: cfg.DataDir}
		// Node↔node forwarding (§4.1/G3.8): the internal mTLS endpoint
		// on :7443 hosts the leader-side ForwardServer; this node's
		// forwarder dials it when it needs the leader to apply/linearize.
		fwd := raftstore.NewGRPCForwarder(
			func() string { return rs.Leader() },
			func(raftAddr string) (string, bool) {
				host, _, err := net.SplitHostPort(raftAddr)
				if err != nil {
					return "", false
				}
				// raft :7444 ↔ internal :7443, same host.
				return net.JoinHostPort(host, fmt.Sprintf("%d", config.PortAPI)), true
			},
		)
		// mTLS dial creds: the same node identity + CN-membership
		// check as the server side. Built synchronously — a forwarded
		// write can happen the instant the store opens, and a plaintext
		// dial against the mTLS listener would be rejected (and cached).
		tlsCfg, err := control.InternalClientTLS(context.Background(), rs, clusterCA, cfg.DataDir)
		if err != nil {
			_ = rs.Close()
			return nil, fmt.Errorf("forwarder mTLS setup: %w", err)
		}
		fwd.SetDialCreds(credentials.NewTLS(tlsCfg))
		rs.SetForwarder(fwd.Forward)
		rs.SetReadForwarder(fwd)
		ctl.fwd = fwd
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
	nixDriver := nix.New()
	r.Register(nixman.New(nixDriver, os.Stdout))
	// Exvol volume attach/mount (T14 §4.7): wait for the device,
	// format only if blank (never reformat), mount noatime.
	r.Register(expmount.New(nil, ""))
	a.recon = r

	// DRBD volume stack: cluster nodes with a configured volume group only.
	if cfg.StorageVG != "" {
		a.initStorage(cfg, st, logger)
	}

	// Block API (T20.5a): served on the agent socket when a block
	// catalog is configured. Admission runs the full V1–V24 rules with
	// the real catalog; NodeCount comes from the store's node records.
	if cfg.BlocksCatalog != "" {
		cat, err := catalog.Load(cfg.BlocksCatalog)
		if err != nil {
			logger.Warn("block catalog unavailable; block API disabled", "dir", cfg.BlocksCatalog, "err", err)
		} else {
			blk := service.New(st, func() validate.Context {
				return validate.Context{Catalog: cat, NodeCount: a.nodeCount()}
			})
			blk.Nodes = wire.Nodes(st)
			a.blocks = blk
			a.blockCatalog = service.NewCatalogServer(cat)
			logger.Info("block API enabled", "types", len(cat.Types()))

			// Replica runtime (T20.5b): converge block-replica desired
			// state into systemd units. Needs both the catalog (type
			// resolution) and a flake ref (closure builds).
			if cfg.BlocksFlakeRef != "" {
				if api, err := systemd.NewDBUSAPI(context.Background()); err != nil {
					logger.Warn("systemd bus unavailable; block replicas disabled", "err", err)
				} else {
					cache, err := systemd.NewCache(filepath.Join(cfg.DataDir, "blocks", "closure-cache.json"))
					if err != nil {
						logger.Warn("closure cache unavailable; block replicas disabled", "err", err)
					} else {
						r.Register(systemd.NewManager(api, &systemd.Applier{
							Cache:   cache,
							Builder: &blockBuilder{driver: nixDriver, flakeRef: cfg.BlocksFlakeRef, logger: logger},
						}))
						logger.Info("block replica runtime enabled", "flake", cfg.BlocksFlakeRef)
					}
				}
			}
		}
	}

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

// nodeCount counts node records for admission (V12/V16 need a node
// count; the adapter's NodeView list would need a context).
func (a *Agent) nodeCount() int {
	entries, err := a.store.List(store.WithStale(context.Background()), "/nodes/")
	if err != nil {
		return 1
	}
	n := 0
	for _, e := range entries {
		rest := strings.TrimPrefix(string(e.Key), "/nodes/")
		if rest != "" && !strings.Contains(rest, "/") {
			n++
		}
	}
	if n == 0 {
		n = 1
	}
	return n
}

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

	// Node liveness lease (§4.3 machinery): each node holds a
	// renewable lease for its own ID. The storage controller judges
	// mesh liveness by its expiry, so a HARD-killed node — which never
	// gets to unpublish its mesh record — leaves the mesh view when
	// the lease expires. Renewal is automatic (Held.renewLoop), and
	// Maintain re-acquires after any loss: one failed renewal (a raft
	// leader change) ends a Held for good, and a node left without the
	// lease reads as dead to the controller, which then finds no candidates.
	//
	// A node that hard-crashed and restarted races its OWN prior
	// lease: that record is still "held" (by this same node ID) until
	// its TTL lapses, since a qemu-quit-style crash never released it.
	// A single TryAcquire hits that conflict once and gives up —
	// permanently, since nothing here ever retries — so the node
	// never renews its own liveness record again and eventually reads
	// as dead to the rest of the cluster forever, despite being
	// healthy. Acquire (blocking, retries on conflict) waits out that
	// stale lease's remaining TTL instead; it runs in its own
	// goroutine so Run() doesn't block on it.
	if a.ctl != nil && a.ctl.store != nil && !witness {
		lm := lease.NewManager(a.ctl.store, a.cfg.NodeID)
		go lm.Maintain(ctx, "node-"+a.cfg.NodeID, 30*time.Second, func(hl *lease.Held) {
			a.nodeLease = hl
			a.logger.Info("node liveness lease held")
		})
	}

	// DRBD volume node loop. Its stop demotes every primary, so shutdown waits for
	// it, and before the store closes: releasing a volume lease needs the store.
	stopVolumes := func() {}
	if a.volnode != nil {
		stopVolumes = a.runVolumes(ctx, cancel)
	}
	if a.volctl != nil {
		go a.volctl.Run(ctx)
	}

	// WireGuard mesh (§4.1): identity + exp0 peer reconciliation from
	// the store. Cluster nodes only; witnesses skip (zero capacity).
	if a.ctl != nil && !witness {
		go a.meshLoop(ctx)
	}

	// VIP holders (§4.2). Cluster nodes only; witnesses never host
	// replicas so they are never candidates.
	if a.ctl != nil && !witness {
		// The LB pool must exist before lbPoolLoop/vipLoop start:
		// both goroutines dereference it (a nil pool segfaults the
		// first watch).
		a.initLB()
		go a.lbPoolLoop(ctx)
		go a.vipLoop(ctx)
		a.initDNS(ctx)
	}
	if a.ctl != nil && a.cfg.Firewall && !witness {
		a.initFirewall(ctx)
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

	// Block placement controller + desired-state bridge (T20.5b):
	// leader-only (Reconcile/Sync self-gate on IsLeader), never on
	// witnesses (§4.9). The store watches Wake the controller; the
	// 30 s interval is the backstop. Nodes view from the wire adapter.
	if a.blocks != nil && !witness {
		if rs, ok := a.store.(*raftstore.Store); ok {
			a.blockCtl = controller.New(rs, wire.Nodes(a.store))
			a.blockCtl.Logger = a.logger
			if a.cfg.ControllerPeriod > 0 {
				a.blockCtl.Interval = a.cfg.ControllerPeriod
			}
			go a.blockCtl.Run(ctx)
			a.blockBridge = &wire.Bridge{St: rs}
			go a.blockBridge.Run(ctx)
			// Fast path: block or node changes wake placement.
			for _, prefix := range []string{"/blocks/", "/nodes/"} {
				p := prefix
				if ch, err := rs.Watch(ctx, store.Key(p), 0); err == nil {
					go func() {
						for range ch {
							a.blockCtl.Wake()
						}
					}()
				}
			}
			a.logger.Info("block controller enabled (leader decides)")
		}
	}

	// Cluster mode: the internal mTLS endpoint on :7443 — the
	// leader-side forwarding service every node runs (G3.8). All
	// nodes need it: any follower may need to forward, any follower
	// may become leader.
	if a.ctl != nil {
		go func() {
			if err := control.ServeInternalEndpoint(ctx, a.ctl.store, a.ctl.ca, a.ctl.dataDir); err != nil {
				a.logger.Error("internal endpoint failed", "err", err)
			}
		}()
		defer func() { _ = a.ctl.fwd.Close() }()
	}

	// gRPC on the unix socket (filesystem permissions are the auth).
	srv := api.NewServer(a, a.store, a.logger)
	srv.Blocks = a.blocks
	srv.Catalog = a.blockCatalog
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
			stopVolumes()
			return fmt.Errorf("grpc serve: %w", err)
		}
	}
	a.status.Store("shutting-down")
	stopVolumes()
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

// raftLogWriter feeds hashicorp raft's pre-formatted log lines (one per
// Write) into the agent's structured logger at warn level.
type raftLogWriter struct {
	log *slog.Logger
}

func (w raftLogWriter) Write(p []byte) (int, error) {
	w.log.Warn("raft", "msg", strings.TrimSpace(string(p)))
	return len(p), nil
}

// blockBuilder adapts the nix ExecDriver to the block runtime's Builder
// interface: attr per type is "<category>-<name>" (slashes are not valid
// in flake attrs), e.g. "util/echo" -> <BlocksFlakeRef>#util-echo.
type blockBuilder struct {
	driver   *nix.ExecDriver
	flakeRef string
	logger   *slog.Logger
}

func (b *blockBuilder) Build(ctx context.Context, blockType string) (nix.StorePath, error) {
	attr := strings.ReplaceAll(blockType, "/", "-")
	p, err := b.driver.Build(ctx, b.flakeRef, attr, os.Stdout)
	if err != nil {
		b.logger.Error("block closure build failed", "type", blockType, "attr", attr,
			"flake", b.flakeRef, "cause", fmt.Sprint(err))
	}
	return p, err
}
