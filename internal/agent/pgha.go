package agent

// PostgreSQL primary election (PHASE-05-TASKS.md Stream A, D1): scans
// block placements for db/postgres instances this node hosts a
// replica of and runs the lease-gated election controller
// (internal/blocks/pgha) against them. Parallel in shape to
// vipPass/vipLoop, but the controller does its own store I/O (lease
// acquire/inspect) rather than needing platform seams.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/expanse/expanse/internal/blocks/blockkey"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/expanse/expanse/internal/blocks/controller"
	"github.com/expanse/expanse/internal/blocks/pgha"
	"github.com/expanse/expanse/internal/blocks/runtime/systemd"
	"github.com/expanse/expanse/internal/cluster/lease"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// pgScanInterval matches vipScanInterval: a role decision, once
// written, is never revisited, so there is no failover-latency budget
// to tune this against — it only bounds how quickly a freshly placed
// replica gets electioned.
const pgScanInterval = vipScanInterval

// pgDefaultPort is db/postgres's defaults.yaml port value, used only
// when a deployed instance's spec.config omits it — admission (V19)
// validates spec.config as submitted, it does not merge defaults.yaml
// first (catalog.Defaults is not consumed anywhere in that path), and
// no generic config+defaults merge exists yet in the block build
// path either. Tracks nix/blocks/db/postgres/defaults.yaml; a mismatch
// here would misroute replica connections, not fail closed, so keep
// it in sync by hand until that generic merge exists.
const pgDefaultPort = 5432

func (a *Agent) initPG() {
	if a.pgCtl != nil {
		return
	}
	a.pgCtl = pgha.New(pgha.Config{
		Leases:      lease.NewManager(a.store, a.cfg.NodeID),
		Self:        a.cfg.NodeID,
		ResolveAddr: a.lookupNodeIP,
		Promote:     a.pgPromote,
		Reconfigure: a.pgReconfigure,
		Confirm:     a.pgConfirm,
		Logger:      a.logger.With("component", "pgha"),
	})
}

func (a *Agent) pgLoop(ctx context.Context) {
	a.initPG()
	tick := time.NewTicker(pgScanInterval)
	defer tick.Stop()
	for {
		a.pgCtl.Pass(ctx, a.scanPostgresInstances(ctx))
		select {
		case <-ctx.Done():
			a.pgCtl.Stop()
			return
		case <-tick.C:
		}
	}
}

// scanPostgresInstances lists /blocks/, keeping only db/postgres
// instances with a RUNNING placement on this node. A failed list
// resolves to "nothing to reconcile this pass" — the same
// don't-flap-on-a-blip choice vipPass makes (internal/agent/vip.go):
// an already-elected instance has nothing to lose from a skipped
// scan, and a not-yet-elected one simply waits one more tick.
func (a *Agent) scanPostgresInstances(ctx context.Context) map[string]pgha.Instance {
	out := map[string]pgha.Instance{}
	ents, err := a.store.List(ctx, store.Key("/blocks/"))
	if err != nil {
		return out
	}
	for _, e := range ents {
		k := string(e.Key)
		if !blockkey.IsSpec(e.Key) {
			continue
		}
		var b pb.Block
		if err := proto.Unmarshal(e.Value, &b); err != nil {
			continue
		}
		if b.GetSpec().GetType() != pgha.BlockType {
			continue
		}
		if se, err := a.store.Get(ctx, store.Key(k+"/status")); err == nil {
			var st pb.BlockStatus
			if err := proto.Unmarshal(se.Value, &st); err == nil {
				b.Status = &st
			}
		}
		// Any live placement counts, not just RUNNING: RUNNING itself
		// requires the workload to already be up, but db/postgres's own
		// workload can only ever start once pgha has written a role
		// decision -- gating the scan on RUNNING is a real chicken-and-
		// egg deadlock (found running the X1 VM test: the only reason
		// election ever succeeded at all was systemd transiently
		// reporting a crash-looping unit "active" long enough for an
		// unrelated reconcile tick to catch it, not a real readiness
		// signal a losing replica could rely on).
		idx := int32(-1)
		for _, pl := range b.GetStatus().GetPlacements() {
			if pl.GetNodeId() == a.cfg.NodeID && pl.GetReplicaIndex() >= 0 && pl.GetPhase() != pb.Phase_LOST {
				idx = pl.GetReplicaIndex()
				break
			}
		}
		if idx < 0 {
			continue
		}
		ref := k[len("/blocks/"):]
		mount := a.pgReplicaMountPath(ctx, ref, int(idx))
		if mount == "" {
			a.logger.Warn("pgha: replica's real mount path not wired yet, retrying next pass",
				"block", ref, "node", a.cfg.NodeID, "index", idx)
			continue
		}
		port := int32(pgDefaultPort)
		if v, ok := b.GetSpec().GetConfig().GetFields()["port"]; ok {
			port = int32(v.GetNumberValue())
		}
		// replicationPassword: required by admission (V19) on every
		// db/postgres block, so its absence here means the field itself
		// changed shape, not a legitimate empty case -- ReplPassword
		// simply comes back "", and Reconfigure fails loudly rather than
		// silently misconnecting.
		replPassword := b.GetSpec().GetConfig().GetFields()["replicationPassword"].GetStringValue()
		out[ref] = pgha.Instance{BlockRef: ref, MountPath: mount, Port: port, Index: idx, ReplPassword: replPassword}
	}
	return out
}

// pgReplicaMountPath resolves a replica's REAL host mount path: the same
// one bridge.go's replicaSpec hands the workload via "--mount", not the
// block's own declared spec.storage[].mountPath (a sandbox-only virtual
// path meaningless to this unsandboxed agent process — a bug found
// running the X1 VM test: pgha was writing its role file under the
// declared path, a plain, unrelated directory on the node's root
// filesystem, while the workload read it from the real per-replica
// volume, so the two never actually met). Reads the desired-state
// record bridge.go already wrote for this node+replica rather than
// re-deriving the volume name/id chain independently, so there is only
// one place that computation can drift.
func (a *Agent) pgReplicaMountPath(ctx context.Context, blockRef string, idx int) string {
	slash := strings.Index(blockRef, "/")
	if slash < 0 {
		return ""
	}
	ns, name := blockRef[:slash], blockRef[slash+1:]
	key := a.recon.DesiredPrefix() + store.Key(controller.ReplicaResourceID(ns, name, idx))
	e, err := a.store.Get(ctx, key)
	if err != nil {
		return ""
	}
	_, payload, ok := bytes.Cut(e.Value, []byte("\n"))
	if !ok {
		return ""
	}
	var spec systemd.Spec
	if err := json.Unmarshal(payload, &spec); err != nil {
		return ""
	}
	for i := 0; i+1 < len(spec.Args); i++ {
		if spec.Args[i] != "--mount" {
			continue
		}
		if _, path, ok := strings.Cut(spec.Args[i+1], "="); ok {
			if !checkDistinctMount(path) {
				// The mount-attach resource (internal/storage/mount)
				// converges independently of — and can lag behind —
				// the desired-state record naming this path
				// (cmd/expanse-block-run's waitForMount documents the
				// same race in full): writing a role file here before
				// the real filesystem actually lands writes into the
				// plain pre-mount host directory instead, which the
				// mount then shadows, silently hiding it forever. Wait
				// for the same "distinct st_dev" signal the workload's
				// own waitForMount blocks on, not just "the record
				// names a path."
				return ""
			}
			return path
		}
	}
	return ""
}

// checkDistinctMount reports whether path is a real, distinct mounted
// filesystem (a different st_dev than its parent directory) — the same
// check cmd/expanse-block-run's waitForMount blocks on, duplicated here
// rather than imported (that package is main, not a library) since this
// agent-side check only needs one snapshot, not a polling wait. A
// package var, not a plain func, so tests can substitute a fake without
// needing a real mount on disk.
var checkDistinctMount = func(path string) bool {
	var pst, cst syscall.Stat_t
	if syscall.Stat(filepath.Dir(path), &pst) != nil {
		return false
	}
	if syscall.Stat(path, &cst) != nil {
		return false
	}
	return cst.Dev != pst.Dev
}

// pgPromoteTimeout bounds one pg_promote() attempt (Stream B, X2). Well
// under pgha.LeaseTTL's own renewal budget: a promotion stuck longer
// than this should give up and let the lease expire for another
// replica to try, not sit there holding a lease with nothing actually
// promoted behind it.
const pgPromoteTimeout = 20 * time.Second

// runPromoteSQL execs one SQL command against a replica's own local
// unix socket and returns its trimmed stdout. A package var, not a
// plain func, so tests can fake it without a real postgres instance —
// the same seam pattern checkDistinctMount already uses.
var runPromoteSQL = func(ctx context.Context, sockDir string, port int32) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "psql",
		"-h", sockDir, "-p", strconv.Itoa(int(port)),
		"-U", "postgres", "-d", "postgres",
		"-tAc", "SELECT pg_promote(true, 15);")
	return cmd.CombinedOutput()
}

// pgPromote is pgha.Config's Promote seam (Stream B, X2): pg_promote()
// over a plain SQL connection to the replica's own local unix socket,
// not a network address — reachable unauthenticated under pg_hba.conf's
// "local all all trust" rule (pgHBAConf's own comment in
// cmd/expanse-block-run/workloads.go already anticipates this exact
// use). wait=true (the first pg_promote() argument) blocks until
// postgres has actually finished switching timelines and is accepting
// writes, so a nil return here means this node genuinely is primary
// now, not just that the request was sent.
func (a *Agent) pgPromote(inst pgha.Instance) error {
	sockDir := filepath.Join(inst.MountPath, ".expanse-postgres", "sock")
	ctx, cancel := context.WithTimeout(context.Background(), pgPromoteTimeout)
	defer cancel()
	out, err := runPromoteSQL(ctx, sockDir, inst.Port)
	trimmed := bytes.TrimSpace(out)
	if err != nil {
		return fmt.Errorf("pg_promote: %w: %s", err, trimmed)
	}
	if !bytes.Equal(trimmed, []byte("t")) {
		return fmt.Errorf("pg_promote: server reported %q, want \"t\"", trimmed)
	}
	return nil
}

// pgConfirmTimeout bounds one confirmed-primary store publish. A plain
// Put, not a SQL round-trip like pgPromoteTimeout's callers, so a much
// tighter budget is enough.
const pgConfirmTimeout = 5 * time.Second

// pgConfirmedPrimaryKey is the store key internal/proxy's own pool
// watches for db/postgres's confirmed-primary record (its own
// confirmedPrimaryPrefix constant is the duplicated-not-imported
// convention's other half). ref is "namespace/name". A dedicated
// top-level prefix, not nested under /blocks/ -- an earlier version put
// it at "/blocks/<ref>/status/pg-primary-confirmed", found running the
// Stream D vertical-slice VM test to break internal/blocks/controller's
// own /blocks/ reconcile scan, which expects every entry there to be
// one of its own recognized shapes.
func pgConfirmedPrimaryKey(ref string) store.Key {
	return store.Key("/pg-primary-confirmed:" + ref)
}

// pgConfirm is pgha.Config's Confirm seam (Stream D): publishes this
// node's own ID as ref's confirmed db/postgres primary, closing the
// race internal/proxy's own primaryConfirmedSuffix doc comment
// describes in full -- winning the election lease alone is not
// sufficient proof of being primary, only that cfg.Promote was about to
// be attempted. Called every pass this node believes itself primary
// (pgha's maintainActive), so a single lost write self-heals on the
// next pass rather than leaving the LB permanently refusing to route
// to a genuinely healthy, already-confirmed primary.
func (a *Agent) pgConfirm(ref string) error {
	ctx, cancel := context.WithTimeout(context.Background(), pgConfirmTimeout)
	defer cancel()
	_, err := a.store.Put(ctx, pgConfirmedPrimaryKey(ref), []byte(a.cfg.NodeID))
	return err
}

// runReconfigureSQL execs one or more ;-separated SQL statements over
// psql against host:port, as user (authenticated by password if
// non-empty, otherwise the local "trust" rule pgHBAConf documents), and
// returns its trimmed combined output. sql travels over stdin, not -c:
// found running the X2 VM test, psql's simple-query protocol sends a
// -c string with multiple statements as ONE message, which postgres
// implicitly wraps in a transaction server-side -- fatal for ALTER
// SYSTEM, which (like CREATE DATABASE, VACUUM, ...) refuses to run
// inside one. Reading a script from stdin instead sends each statement
// as its own separate message, client-side, avoiding that wrap
// entirely -- the same delivery cmd/expanse-block-run's own CREATE
// ROLE/CREATE DATABASE bootstrap step already relies on, proven there
// first. A package var, not a plain func, for the same fakeability
// runPromoteSQL and checkDistinctMount already provide.
var runReconfigureSQL = func(ctx context.Context, host string, port int32, user, password, dbname, sql string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "psql",
		"-h", host, "-p", strconv.Itoa(int(port)),
		"-U", user, "-d", dbname,
		"-v", "ON_ERROR_STOP=1")
	cmd.Stdin = strings.NewReader(sql)
	if password != "" {
		cmd.Env = append(cmd.Environ(), "PGPASSWORD="+password)
	}
	return cmd.CombinedOutput()
}

// sqlQuote wraps s as a single-quoted SQL string literal, doubling any
// embedded quote -- the standard SQL escaping rule, needed here because
// the values involved (a resolved node address, a replication password)
// are not literal constants this package controls.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// pgReconfigure is pgha.Config's Reconfigure seam (Stream B, X2):
// retargets this node's own standby at a new primary once pgha has
// learned the primary lease moved there. Two steps, since a fresh
// primary carries no replication slots of its own -- each standby's
// slot only ever existed on whichever node created it:
//
//  1. (Re)create this replica's own slot on the new primary, over the
//     network, as the replicator role -- the same idempotent shape
//     cmd/expanse-block-run's own ensureSlot uses at bootstrap, just
//     against a different (the new) primary.
//  2. Retarget this replica's own local postgres at the new primary and
//     that slot, then reload -- primary_conninfo/primary_slot_name are
//     PGC_SIGHUP GUCs; postgres itself restarts the walreceiver when a
//     reload changes either, no restart of this process required.
func (a *Agent) pgReconfigure(inst pgha.Instance, newHost string) error {
	slash := strings.Index(inst.BlockRef, "/")
	if slash < 0 {
		return fmt.Errorf("pgReconfigure: malformed block ref %q", inst.BlockRef)
	}
	ns, name := inst.BlockRef[:slash], inst.BlockRef[slash+1:]
	slot := pgha.SlotName(ns, name, int(inst.Index))

	ctx, cancel := context.WithTimeout(context.Background(), pgPromoteTimeout)
	defer cancel()

	ensureSlot := fmt.Sprintf(
		"SELECT pg_create_physical_replication_slot(%s) WHERE NOT EXISTS "+
			"(SELECT 1 FROM pg_replication_slots WHERE slot_name = %s);",
		sqlQuote(slot), sqlQuote(slot))
	if out, err := runReconfigureSQL(ctx, newHost, inst.Port, "replicator", inst.ReplPassword, "postgres", ensureSlot); err != nil {
		return fmt.Errorf("ensure slot on new primary: %w: %s", err, bytes.TrimSpace(out))
	}

	sockDir := filepath.Join(inst.MountPath, ".expanse-postgres", "sock")
	conninfo := fmt.Sprintf("host=%s port=%d user=replicator password=%s application_name=expanse",
		newHost, inst.Port, inst.ReplPassword)
	retarget := fmt.Sprintf(
		"ALTER SYSTEM SET primary_conninfo = %s; ALTER SYSTEM SET primary_slot_name = %s; SELECT pg_reload_conf();",
		sqlQuote(conninfo), sqlQuote(slot))
	if out, err := runReconfigureSQL(ctx, sockDir, inst.Port, "postgres", "", "postgres", retarget); err != nil {
		return fmt.Errorf("retarget local standby: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
