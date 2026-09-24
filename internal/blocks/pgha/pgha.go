// Package pgha is the lease-gated PostgreSQL primary-election
// controller (PHASE-05-TASKS.md D1): a thin native reconciler over the
// existing Raft lease, directly parallel in shape to vip.Holder but
// electing which replica of a db/postgres block instance believes
// itself primary rather than which node holds a VIP.
//
// Scope (Stream A / X1 only): initial election. The controller decides
// exactly once per instance which replica becomes primary and writes
// that decision to the file nix/blocks/db/postgres/module.nix's
// bootstrap script consults on a replica's very first start. It keeps
// the winning node's lease held for as long as this agent runs,
// including across an agent restart (reclaimed, not re-elected) —
// laying the groundwork for Stream B's promotion-on-loss, which this
// package does not implement.
package pgha

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/expanse/expanse/internal/cluster/lease"
	"github.com/expanse/expanse/internal/errors"
)

// BlockType is the db/postgres block's spec.type value. Both the
// agent's placement scan (internal/agent/pgha.go) and the LB's
// primary-only routing mode (D2, internal/agent/lb.go) key off this
// constant rather than repeating the literal.
const BlockType = "db/postgres"

// StaticUID is the fixed, non-root uid every db/postgres replica runs
// under (internal/blocks/wire/bridge.go wires it via Spec.StaticUID,
// PHASE-05-TASKS.md Stream A X1). Postgres refuses to run as root
// outright, ruling out RunAsRoot the way share/smb's setuid() needs
// rule out DynamicUser: DynamicUser mints a fresh, unpredictable uid on
// every unit start, not just once per replica — this unit fully exits
// and restarts on any bootstrap hiccup (Restart=on-failure), so a
// directory the workload creates under one restart's uid becomes
// permanently inaccessible to the next restart's different uid,
// confirmed by the X1 VM test: initdb/pg_basebackup's own state
// directory (.expanse-postgres/) hit exactly this, wedged in a
// permission-denied crash loop no amount of waiting ever resolved. A
// single shared uid across every db/postgres replica on a node is
// safe despite not being per-block: each replica's data lives in its
// own dedicated volume, invisible outside that one unit's own bind
// mount regardless of which uid owns it.
const StaticUID = 8332

// RoleFile is the path, relative to a replica's mount point, that
// module.nix's bootstrap script reads on first start.
const RoleFile = ".expanse-postgres/role"

// primaryContent is RoleFile's exact content when this node won
// election — must match what module.nix's `read -r kind host peerport`
// parses as the "primary" case.
const primaryContent = "primary\n"

// LeaseTTL is the primary-election lease TTL. Longer than
// vip.LeaseTTL: losing and reacquiring this lease is not yet wired to
// any promotion action (Stream B), so there is no failover-latency
// budget to bound it against — correctness (one primary at a time)
// is what Stream A needs, not speed.
const LeaseTTL = lease.DefaultTTL

// LeaseName is the lease key electing one node primary for one
// db/postgres block instance.
func LeaseName(blockRef string) string {
	return "pg-primary:" + blockRef
}

// RolePath returns the role-file path under a replica's mount point.
func RolePath(mountPath string) string {
	return filepath.Join(mountPath, RoleFile)
}

// Instance is one db/postgres replica placement a node hosts.
type Instance struct {
	BlockRef  string // block ref, e.g. "default/pg"
	MountPath string // spec.storage[0].mountPath
	Port      int32  // spec.config.port (same for every replica of one instance)
}

// AddrResolver returns a node's routable dial address (host only, no
// port), or "" if unknown.
type AddrResolver func(nodeID string) string

// Config configures a Controller. Leases, Self and ResolveAddr are
// required.
type Config struct {
	Leases      *lease.Manager
	Self        string
	ResolveAddr AddrResolver
	TTL         time.Duration // default LeaseTTL
	Logger      *slog.Logger  // optional; nil disables logging
}

// Controller runs primary election for every db/postgres instance a
// node hosts a replica of.
type Controller struct {
	cfg Config

	mu     sync.Mutex
	active map[string]*lease.Held // blockRef -> primary lease this node holds
}

// New returns a Controller.
func New(cfg Config) *Controller {
	if cfg.TTL <= 0 {
		cfg.TTL = LeaseTTL
	}
	return &Controller{cfg: cfg, active: map[string]*lease.Held{}}
}

func (c *Controller) log(level func(string, ...any), msg string, args ...any) {
	if c.cfg.Logger != nil {
		level(msg, args...)
	}
}

// Pass reconciles every instance in the current set, keyed by
// BlockRef. Instances no longer present have any lease this node was
// maintaining for them released — the replica is gone, nothing left
// to fence.
func (c *Controller) Pass(ctx context.Context, instances map[string]Instance) {
	c.mu.Lock()
	for ref, held := range c.active {
		if _, ok := instances[ref]; !ok {
			c.releaseLocked(ref, held)
		}
	}
	c.mu.Unlock()

	for ref, inst := range instances {
		c.reconcileOne(ctx, ref, inst)
	}
}

// Stop releases every lease this node is maintaining (agent shutdown).
func (c *Controller) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for ref, held := range c.active {
		c.releaseLocked(ref, held)
	}
}

// releaseLocked must be called with c.mu held.
func (c *Controller) releaseLocked(ref string, held *lease.Held) {
	rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.cfg.Leases.Release(rctx, held) //nolint:errcheck // best effort on removal
	delete(c.active, ref)
}

func (c *Controller) reconcileOne(ctx context.Context, ref string, inst Instance) {
	content, err := os.ReadFile(RolePath(inst.MountPath))
	if err == nil {
		c.onDecided(ctx, ref, string(content))
		return
	}
	if !os.IsNotExist(err) {
		c.log(c.cfg.Logger.Warn, "pgha: role file unreadable", "block", ref, "err", err)
		return
	}
	c.elect(ctx, ref, inst)
}

// onDecided keeps a self-elected primary's lease alive across agent
// restarts: a role file already saying "primary" means this node won
// a past election, but the in-memory Held from that run is gone on
// restart — reclaim the record rather than re-electing, since a
// decided role file is never rewritten.
func (c *Controller) onDecided(ctx context.Context, ref, content string) {
	if content != primaryContent {
		return // decided replica: nothing to maintain
	}
	c.mu.Lock()
	_, already := c.active[ref]
	c.mu.Unlock()
	if already {
		return
	}
	// Bounded, not the indefinite retry AcquireReclaiming normally does:
	// a stuck reclaim must not block reconciling this node's OTHER
	// instances in the same Pass. A record that turns out to be held
	// live by someone else (should not happen absent a bug — only the
	// role file's own owner ever attempts this) resolves on the next
	// Pass instead of hanging this one.
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	held, err := c.cfg.Leases.AcquireReclaiming(rctx, LeaseName(ref), c.cfg.TTL)
	if err != nil {
		c.log(c.cfg.Logger.Warn, "pgha: reclaim primary lease failed, retrying next pass", "block", ref, "err", err)
		return
	}
	c.mu.Lock()
	c.active[ref] = held
	c.mu.Unlock()
}

// elect runs the Stream A election: exactly one replica across the
// block's set wins TryAcquire; everyone else reads the winner off the
// lease record and writes "replica <host> <port>".
func (c *Controller) elect(ctx context.Context, ref string, inst Instance) {
	name := LeaseName(ref)
	held, err := c.cfg.Leases.TryAcquire(ctx, name, c.cfg.TTL)
	if err == nil {
		if werr := writeRoleFile(RolePath(inst.MountPath), primaryContent); werr != nil {
			c.log(c.cfg.Logger.Error, "pgha: won election but role file write failed", "block", ref, "err", werr)
			held.Abandon() // do not leave this node fenced as primary with no on-disk decision
			return
		}
		c.mu.Lock()
		c.active[ref] = held
		c.mu.Unlock()
		c.log(c.cfg.Logger.Info, "pgha: elected primary", "block", ref, "node", c.cfg.Self, "roleFile", RolePath(inst.MountPath))
		return
	}
	if !errors.Is(err, errors.KindConflict) {
		c.log(c.cfg.Logger.Warn, "pgha: election attempt failed, retrying next pass", "block", ref, "err", err)
		return
	}
	// Lost the race: read who won. A not-yet-visible record (the
	// winner's write hasn't landed) resolves on the next Pass — do not
	// guess.
	l, ok, ierr := c.cfg.Leases.Inspect(ctx, name)
	if ierr != nil || !ok || l.Holder == "" || l.Holder == c.cfg.Self {
		// Expected and brief in the common case (the winner's record
		// simply hasn't landed yet, or this node is the winner itself
		// re-losing its own already-held lease's fresh TryAcquire) —
		// logged rather than fully silent so a losing replica that
		// never gets a role file at all is diagnosable from which of
		// these cases it's actually stuck on.
		c.log(c.cfg.Logger.Warn, "pgha: lost election, no usable winner record yet", "block", ref, "ierr", ierr, "ok", ok, "holder", l.Holder, "self", c.cfg.Self)
		return
	}
	host := c.cfg.ResolveAddr(l.Holder)
	if host == "" {
		c.log(c.cfg.Logger.Warn, "pgha: primary elected but address unresolved, retrying next pass", "block", ref, "primary", l.Holder)
		return
	}
	if werr := writeRoleFile(RolePath(inst.MountPath), fmt.Sprintf("replica %s %d\n", host, inst.Port)); werr != nil {
		c.log(c.cfg.Logger.Error, "pgha: role file write failed", "block", ref, "err", werr)
		return
	}
	c.log(c.cfg.Logger.Info, "pgha: wrote replica role", "block", ref, "primary", l.Holder, "host", host, "roleFile", RolePath(inst.MountPath))
}

// writeRoleFile creates the state directory and writes the role file
// atomically (temp file + rename) so a crash mid-write can never leave
// module.nix's bootstrap script reading a truncated decision.
//
// This controller runs as root (the agent process), but the
// db/postgres workload reading this same file — and, when it wins,
// creating its own subdirectories (.expanse-postgres/sock) right next
// to it — runs under a fixed non-root uid (pgha.StaticUID,
// PHASE-05-TASKS.md Stream A X1). World-readable/writable is the
// pragmatic fix, not a real exposure: the role content itself is never
// sensitive (just "primary" or "replica <host> <port>"), the only
// writer is ever this controller, and the only process that can even
// see this host path at all is the one replica unit whose own bind
// mount names it. Found via the X1 VM test: whichever of this
// controller or the workload created the directory first left the
// other permanently locked out at the previous 0700/0600 modes.
func writeRoleFile(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	// MkdirAll's own requested mode is masked by this process's umask
	// (typically 0022, stripping exactly the group/other write bits the
	// workload's differing uid needs) — Chmod sets the exact bits,
	// bypassing that, and is safe to run unconditionally whether this
	// call just created the directory or it already existed (e.g. the
	// workload itself created it first).
	if err := os.Chmod(dir, 0o777); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
