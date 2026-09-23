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
		c.log(c.cfg.Logger.Info, "pgha: elected primary", "block", ref, "node", c.cfg.Self)
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
		return
	}
	host := c.cfg.ResolveAddr(l.Holder)
	if host == "" {
		c.log(c.cfg.Logger.Warn, "pgha: primary elected but address unresolved, retrying next pass", "block", ref, "primary", l.Holder)
		return
	}
	if werr := writeRoleFile(RolePath(inst.MountPath), fmt.Sprintf("replica %s %d\n", host, inst.Port)); werr != nil {
		c.log(c.cfg.Logger.Error, "pgha: role file write failed", "block", ref, "err", werr)
	}
}

// writeRoleFile creates the state directory and writes the role file
// atomically (temp file + rename) so a crash mid-write can never leave
// module.nix's bootstrap script reading a truncated decision.
func writeRoleFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
