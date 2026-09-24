// Package pgha is the lease-gated PostgreSQL primary-election
// controller (PHASE-05-TASKS.md D1): a thin native reconciler over the
// existing Raft lease, directly parallel in shape to vip.Holder but
// electing which replica of a db/postgres block instance believes
// itself primary rather than which node holds a VIP.
//
// Scope: initial election (Stream A / X1), automatic failover
// (Stream B / X2), and old-primary demotion on rejoin (Stream C / X4).
// The controller decides exactly once per instance which replica
// becomes primary and writes that decision to the file
// nix/blocks/db/postgres/module.nix's bootstrap script consults on a
// replica's very first start; it keeps the winning node's lease held
// for as long as this agent runs, including across an agent restart
// (reclaimed, not re-elected) — unless that reclaim discovers another
// node already won the lease for real while this one was down or
// partitioned away, in which case it demotes this node's role file to
// "replica" instead (demoteIfLostToAnother). A decided replica never
// stops re-attempting the primary lease on every pass — cheap and
// silent while the primary is alive (an immediate conflict), and on
// the rare pass where the primary's lease has actually expired, the
// winner promotes its own already-streaming replica via cfg.Promote
// (pg_promote(), D4) rather than restarting anything.
package pgha

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

// LeaseTTL is the primary-election lease TTL. Matches vip.LeaseTTL (not
// imported — same "local copy of the convention" decoupling this
// package's other constants already use): losing this lease now drives
// a real failover (Stream B), so the same failover-latency budget
// applies — TTL/3 renewal + slippage bounds how long a dead primary's
// fence outlives it before a standby can promote.
const LeaseTTL = 10 * time.Second

// LeaseName is the lease key electing one node primary for one
// db/postgres block instance.
func LeaseName(blockRef string) string {
	return "pg-primary:" + blockRef
}

// RolePath returns the role-file path under a replica's mount point.
func RolePath(mountPath string) string {
	return filepath.Join(mountPath, RoleFile)
}

// SlotName derives one replica's own physical replication slot name,
// deterministically from its identity. Shared by the workload
// (cmd/expanse-block-run's own pgSlotName, which creates the slot at
// bootstrap against whichever node is primary then) and this package's
// Reconfigure path (Stream B, which must re-create the same slot on
// whichever node becomes primary after a failover, since a fresh
// primary carries no replication slots of its own). Not imported by
// cmd/expanse-block-run (that package is main, not a library) — kept in
// sync by producing byte-identical output for the same replica, proven
// by each package's own test against the same example. Postgres slot
// names must match [a-z0-9_]+.
func SlotName(ns, name string, idx int) string {
	raw := fmt.Sprintf("%s-%s-%d", ns, name, idx)
	var b strings.Builder
	for _, r := range strings.ToLower(raw) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return "expanse_" + b.String()
}

// parseReplicaRole splits a decided replica's role-file content,
// "replica <host> <port>\n", into host and port. Empty strings for any
// other content (including "primary\n", which callers must not pass
// here).
func parseReplicaRole(content string) (host, port string) {
	fields := strings.Fields(content)
	if len(fields) < 3 || fields[0] != "replica" {
		return "", ""
	}
	return fields[1], fields[2]
}

// Instance is one db/postgres replica placement a node hosts.
type Instance struct {
	BlockRef     string // block ref, e.g. "default/pg"
	MountPath    string // spec.storage[0].mountPath
	Port         int32  // spec.config.port (same for every replica of one instance)
	Index        int32  // this replica's own index, for SlotName
	ReplPassword string // spec.config.replicationPassword, for Reconfigure's primary_conninfo
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
	// Promote triggers pg_promote() (D4) against this node's own,
	// already-streaming replica once it has won the primary lease after
	// the previous primary's lease expired (Stream B, X2). Required for
	// failover to ever complete; a nil Promote only ever matters if a
	// replica actually wins that race, which every existing election
	// test avoids by construction (a live or absent-but-unraced lease).
	Promote func(inst Instance) error
	// Reconfigure retargets this node's own standby at a new primary
	// (host) once the primary lease has moved to a different node than
	// whichever one this replica currently streams from (Stream B, X2):
	// without it, a surviving standby keeps trying to reach the dead
	// primary forever, and with synchronous_standby_names active, every
	// write to the new primary blocks forever waiting for a synchronous
	// standby that can never arrive — found running the X2 VM test, not
	// assumed in advance. A nil Reconfigure only matters once a
	// promotion actually happens, the same construction every existing
	// test avoids as Promote's own doc explains.
	Reconfigure func(inst Instance, newHost string) error
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
	c.mu.Lock()
	_, active := c.active[ref]
	c.mu.Unlock()
	if active {
		// The Held's own background renewal loop keeps the lease alive;
		// nothing to do here except catch up a role file a prior
		// promotion couldn't write (promoteOnLoss's own comment explains
		// why that never abandons the lease).
		c.maintainActive(ref, inst)
		return
	}
	content, err := os.ReadFile(RolePath(inst.MountPath))
	if err == nil {
		c.onDecided(ctx, ref, inst, string(content))
		return
	}
	if !os.IsNotExist(err) {
		c.log(c.cfg.Logger.Warn, "pgha: role file unreadable", "block", ref, "err", err)
		return
	}
	c.elect(ctx, ref, inst)
}

// onDecided routes an instance whose role file already names a
// decision: "primary" reclaims this node's own past election across an
// agent restart; anything else is a replica watching for its chance to
// promote (Stream B, X2).
func (c *Controller) onDecided(ctx context.Context, ref string, inst Instance, content string) {
	if content == primaryContent {
		c.reclaimPrimary(ctx, ref, inst)
		return
	}
	c.promoteOnLoss(ctx, ref, inst, content)
}

// reclaimPrimary keeps a self-elected primary's lease alive across
// agent restarts: a role file already saying "primary" means this node
// won a past election, but the in-memory Held from that run is gone on
// restart — reclaim the record rather than re-electing, since a
// decided role file is never rewritten by this path.
//
// A reclaim can also lose for real (Stream C, X4): this node was down
// or partitioned long enough that another replica's own promotion
// (promoteOnLoss) already won the lease, so the record AcquireReclaiming
// finds live is no longer this node's own. That is not a blip to retry
// — it is this node discovering it is now a stale, diverged ex-primary,
// handled by demoteIfLostToAnother.
func (c *Controller) reclaimPrimary(ctx context.Context, ref string, inst Instance) {
	// Bounded, not the indefinite retry AcquireReclaiming normally does:
	// a stuck reclaim must not block reconciling this node's OTHER
	// instances in the same Pass. AcquireReclaiming's own internal loop
	// (lease.Manager.acquire) already retries a live-elsewhere conflict
	// silently for this whole budget, surfacing it to us as a timeout,
	// not errors.KindConflict — so the error's kind cannot distinguish
	// "someone else genuinely holds it" from a plain store hiccup here;
	// demoteIfLostToAnother's own Inspect call is what actually tells
	// those apart, on ANY failure of this call.
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	held, err := c.cfg.Leases.AcquireReclaiming(rctx, LeaseName(ref), c.cfg.TTL)
	if err == nil {
		c.mu.Lock()
		c.active[ref] = held
		c.mu.Unlock()
		return
	}
	c.demoteIfLostToAnother(ctx, ref, inst)
}

// demoteIfLostToAnother runs once reclaimPrimary's AcquireReclaiming has
// lost to a live holder that is not this node: definitive proof another
// replica already promoted while this one was unreachable, not a
// not-yet-landed record (Inspect resolving to "" or to this node itself
// is exactly that ambiguous case, and is left to retry next pass rather
// than acted on).
//
// The only action taken here is rewriting the role file to
// "replica <host> <port>" — the same content elect()'s losing branch
// writes for a brand-new replica. That is deliberate: this package never
// touches postgres itself or the node's own PGDATA (Promote/Reconfigure
// are the only seams that do, and both require inst's Instance data this
// call already has, but a demotion needs neither). Stopping the stale
// primary and wiping its diverged data directory belongs to
// cmd/expanse-block-run's own workload process, which already owns that
// directory's whole lifecycle end to end — this file is simply the
// signal its demote-watch goroutine waits for.
func (c *Controller) demoteIfLostToAnother(ctx context.Context, ref string, inst Instance) {
	l, ok, ierr := c.cfg.Leases.Inspect(ctx, LeaseName(ref))
	if ierr != nil || !ok || l.Holder == "" || l.Holder == c.cfg.Self {
		return // not a definitive loss yet -- retry next pass
	}
	host := c.cfg.ResolveAddr(l.Holder)
	if host == "" {
		return // retry next pass
	}
	if werr := writeRoleFile(RolePath(inst.MountPath), fmt.Sprintf("replica %s %d\n", host, inst.Port)); werr != nil {
		c.log(c.cfg.Logger.Error, "pgha: demoted but role file write failed, retrying next pass", "block", ref, "err", werr)
		return
	}
	c.log(c.cfg.Logger.Warn, "pgha: primary lease held by another live node, demoting to replica", "block", ref, "primary", l.Holder, "host", host)
}

// promoteOnLoss is Stream B's failover path: a decided replica retries
// TryAcquire on the primary lease every pass instead of ever going
// quiet. In the steady state (the primary is alive and renewing) this
// is an immediate, cheap conflict — the exact cost elect() already
// pays win-or-lose, just repeated. Only once the dead primary's lease
// has actually expired does TryAcquire succeed here, at which point
// this replica's own already-streaming postgres is promoted via
// cfg.Promote before the role file is rewritten to match.
//
// Losing the race is not always a pure no-op, unlike Stream A's elect():
// the winner may be a DIFFERENT node than the one this replica's own
// role file still names, meaning a previous promotion moved the primary
// out from under it. Retargeting via cfg.Reconfigure is required for
// correctness, not just completeness — with synchronous_standby_names
// active, a standby that never reconnects means the new primary can
// never complete a synchronous commit at all (found running the X2 VM
// test: promotion itself succeeded, but every client write past that
// point hung, because the surviving standby was still streaming from
// the now-dead primary and no synchronous standby could ever ack).
func (c *Controller) promoteOnLoss(ctx context.Context, ref string, inst Instance, content string) {
	held, err := c.cfg.Leases.TryAcquire(ctx, LeaseName(ref), c.cfg.TTL)
	if err == nil {
		c.log(c.cfg.Logger.Warn, "pgha: primary lease free, promoting this replica", "block", ref, "node", c.cfg.Self)
		if c.cfg.Promote == nil {
			c.log(c.cfg.Logger.Error, "pgha: won promotion race but no Promote seam configured", "block", ref)
			held.Abandon()
			return
		}
		if perr := c.cfg.Promote(inst); perr != nil {
			c.log(c.cfg.Logger.Error, "pgha: pg_promote failed, abandoning lease", "block", ref, "err", perr)
			held.Abandon() // nothing was actually promoted; do not stay fenced as primary
			return
		}
		c.mu.Lock()
		c.active[ref] = held
		c.mu.Unlock()
		c.log(c.cfg.Logger.Info, "pgha: promoted to primary", "block", ref, "node", c.cfg.Self)
		c.maintainActive(ref, inst)
		return
	}
	if !errors.Is(err, errors.KindConflict) {
		c.log(c.cfg.Logger.Warn, "pgha: promotion lease attempt failed, retrying next pass", "block", ref, "err", err)
		return
	}
	c.retargetIfPrimaryMoved(ctx, ref, inst, content)
}

// retargetIfPrimaryMoved runs after losing the primary-lease race to a
// live holder: learns who currently holds it, and — only if that
// differs from the host this replica's own role file already names —
// retargets this node's local standby via cfg.Reconfigure and rewrites
// the role file to match. A no-op in the overwhelmingly common case
// (the original primary is still alive and still who we're streaming
// from), so this costs one extra Inspect per pass, not a reconfigure.
func (c *Controller) retargetIfPrimaryMoved(ctx context.Context, ref string, inst Instance, content string) {
	l, ok, ierr := c.cfg.Leases.Inspect(ctx, LeaseName(ref))
	if ierr != nil || !ok || l.Holder == "" || l.Holder == c.cfg.Self {
		return // nothing new to learn this pass
	}
	host := c.cfg.ResolveAddr(l.Holder)
	if host == "" {
		return // retry next pass
	}
	if curHost, _ := parseReplicaRole(content); curHost == host {
		return // already tracking the current primary
	}
	// A replica that has never finished its OWN first-ever bootstrap has
	// no local postgres for cfg.Reconfigure to retarget at all -- calling
	// it would just fail forever on a missing local socket (found running
	// the X3/X4 VM test: a third replica still mid-bootstrap when the
	// primary died stayed permanently wedged retrying against the dead
	// node, since Reconfigure's own failure is exactly what gates the
	// role-file rewrite below). Skip straight to it instead: nothing has
	// bootstrapped yet, so nothing needs retargeting -- the workload's
	// own next bootstrap attempt reads this fresh and clones directly
	// from the new primary.
	if !hasBootstrapped(inst.MountPath) {
		if werr := writeRoleFile(RolePath(inst.MountPath), fmt.Sprintf("replica %s %d\n", host, inst.Port)); werr != nil {
			c.log(c.cfg.Logger.Error, "pgha: primary moved before this replica ever bootstrapped, but role file write failed, retrying next pass", "block", ref, "err", werr)
			return
		}
		c.log(c.cfg.Logger.Warn, "pgha: primary moved before this replica ever bootstrapped, retargeting its still-pending first clone", "block", ref, "primary", l.Holder, "host", host)
		return
	}
	if c.cfg.Reconfigure == nil {
		c.log(c.cfg.Logger.Error, "pgha: primary moved but no Reconfigure seam configured", "block", ref, "primary", l.Holder)
		return
	}
	if rerr := c.cfg.Reconfigure(inst, host); rerr != nil {
		c.log(c.cfg.Logger.Error, "pgha: retargeting streaming replication failed, retrying next pass", "block", ref, "primary", l.Holder, "err", rerr)
		return
	}
	if werr := writeRoleFile(RolePath(inst.MountPath), fmt.Sprintf("replica %s %d\n", host, inst.Port)); werr != nil {
		c.log(c.cfg.Logger.Error, "pgha: retargeted but role file write failed, retrying next pass", "block", ref, "err", werr)
		return
	}
	c.log(c.cfg.Logger.Info, "pgha: retargeted streaming replication to new primary", "block", ref, "primary", l.Holder, "host", host)
}

// pgdataSubdir is cmd/expanse-block-run's own fixed pgdata subdirectory
// name under a replica's mount point (workloads.go's runPostgres), kept
// in sync here by duplication -- the same "not imported, main vs
// library" boundary SlotName's own doc comment explains.
const pgdataSubdir = "pgdata"

// hasBootstrapped reports whether a replica's own PGDATA has ever been
// initialized (PG_VERSION present), the same check runPostgres itself
// uses to decide whether to bootstrap. A pure filesystem read, not a
// network or lease call, so it costs nothing extra on the common path.
func hasBootstrapped(mountPath string) bool {
	_, err := os.Stat(filepath.Join(mountPath, pgdataSubdir, "PG_VERSION"))
	return err == nil
}

// maintainActive keeps the on-disk role file in step with a lease this
// node already, definitely holds. It is never the reason a lease is
// abandoned: postgres has already been promoted (or was elected primary
// to begin with) by the time this runs, so a write failure here — the
// state directory's mount lagging, e.g. — is retried next pass, not
// treated as a reason to give up the fence (D5: relinquishing a lease
// this node's own postgres already believes it holds risks a second
// replica promoting too).
func (c *Controller) maintainActive(ref string, inst Instance) {
	path := RolePath(inst.MountPath)
	if content, err := os.ReadFile(path); err == nil && string(content) == primaryContent {
		return
	}
	if werr := writeRoleFile(path, primaryContent); werr != nil {
		c.log(c.cfg.Logger.Warn, "pgha: role file not yet writable, retrying next pass", "block", ref, "err", werr)
	}
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
