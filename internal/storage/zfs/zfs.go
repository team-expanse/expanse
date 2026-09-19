// Package zfs wraps the zfs/zpool commands for exvol's storage engine
// (Phase 06 §4.5). libzfs bindings are a cgo liability; shelling out with
// -H -p -o parseable output is fine and testable, per the spec.
//
// This is the only package in the tree permitted to exec zfs/zpool
// directly — everything else goes through the ZFS interface here.
package zfs

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strconv"
	"strings"

	experrors "github.com/expanse/expanse/internal/errors"
)

// Default zvol properties for exvol volumes (§4.5): 16k volblocksize
// matches most DB page sizes better than the 8k default, sync=always for
// durability, logbias=throughput and primarycache=metadata because the
// block's own cache is better placed. All are overridable per storage
// class via the props map.
var DefaultZvolProps = map[string]string{
	"volblocksize": "16k",
	"compression":  "zstd",
	"sync":         "always",
	"logbias":      "throughput",
	"primarycache": "metadata",
}

// PoolDevice is one vdev row from `zpool status`.
type PoolDevice struct {
	Name  string
	State string // ONLINE, DEGRADED, FAULTED, OFFLINE, UNAVAIL, REMOVED
}

// PoolStatus is the health snapshot the volume controller's pool monitor
// needs (§4.5): DEGRADED → volume degraded event; FAULTED → evacuate.
type PoolStatus struct {
	Health  string // pool-level state, first status row
	Devices []PoolDevice
	Errors  uint64 // data error count; 0 means "no known data errors"
}

// ZFS is the storage engine's view of the local ZFS pool (§4.5).
type ZFS interface {
	CreateZvol(ctx context.Context, name string, size uint64, props map[string]string) error
	DestroyZvol(ctx context.Context, name string, recursive bool) error
	Snapshot(ctx context.Context, dataset, snap string) error
	DestroySnapshot(ctx context.Context, dataset, snap string) error
	ListSnapshots(ctx context.Context, dataset string) ([]string, error)
	Send(ctx context.Context, dataset, from, to string, w io.Writer) error
	Receive(ctx context.Context, dataset string, r io.Reader) error
	Resize(ctx context.Context, zvol string, size uint64) error
	Rollback(ctx context.Context, dataset, snap string) error
	PoolStatus(ctx context.Context, pool string) (*PoolStatus, error)
	Scrub(ctx context.Context, pool string) error
}

// Exec implements ZFS by shelling out to zfs/zpool. The runCmd hook is the
// same injection idiom internal/agent/nix uses, so tests can substitute
// fake binaries without cgo or root.
type Exec struct {
	ZfsPath   string // default "zfs"
	ZpoolPath string // default "zpool"
	runCmd    func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// New creates the default exec-backed driver.
func New() *Exec {
	return &Exec{
		ZfsPath:   "zfs",
		ZpoolPath: "zpool",
		runCmd: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, name, args...)
		},
	}
}

func (e *Exec) cmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	if e.runCmd != nil {
		return e.runCmd(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}

// run executes a command, returning combined stderr (for error
// classification) on failure.
func (e *Exec) run(ctx context.Context, op, name string, args ...string) ([]byte, error) {
	cmd := e.cmd(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, execErr(ctx, op, name, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// runWithIO executes a command wiring caller-supplied stdin/stdout (Send /
// Receive), capturing stderr for error classification.
func (e *Exec) runWithIO(ctx context.Context, op, name string, stdin io.Reader, stdout io.Writer, args ...string) error {
	cmd := e.cmd(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return execErr(ctx, op, name, err, stderr.String())
	}
	return nil
}

func execErr(ctx context.Context, op, name string, err error, stderr string) error {
	if ctx.Err() != nil {
		return experrors.Wrap(ctx.Err(), experrors.KindTimeout, op, "canceled")
	}
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = err.Error()
	}
	kind := experrors.KindInternal
	if strings.Contains(msg, "does not exist") || strings.Contains(msg, "no such pool or dataset") {
		kind = experrors.KindNotFound
	}
	return experrors.Wrap(err, kind, op, msg)
}

// CreateZvol creates a zvol of the given size (bytes). Default exvol
// properties (§4.5) are applied first; props overrides them per storage
// class.
// Mountpoint returns the mountpoint of a dataset ("" when unmounted).
func (e *Exec) Mountpoint(ctx context.Context, dataset string) (string, error) {
	out, err := e.run(ctx, "get", e.ZfsPath, "get", "-H", "-o", "value", "mountpoint", dataset)
	if err != nil {
		return "", err
	}
	mp := strings.TrimSpace(string(out))
	if mp == "-" || mp == "none" {
		return "", nil
	}
	return mp, nil
}

func (e *Exec) CreateZvol(ctx context.Context, name string, size uint64, props map[string]string) error {
	merged := make(map[string]string, len(DefaultZvolProps)+len(props))
	for k, v := range DefaultZvolProps {
		merged[k] = v
	}
	for k, v := range props {
		merged[k] = v
	}
	args := []string{"create", "-V", strconv.FormatUint(size, 10)}
	if bs, ok := merged["volblocksize"]; ok {
		args = append(args, "-b", bs)
		delete(merged, "volblocksize")
	}
	for _, k := range sortedKeys(merged) {
		args = append(args, "-o", k+"="+merged[k])
	}
	args = append(args, name)
	_, err := e.run(ctx, "zfs.CreateZvol", e.ZfsPath, args...)
	return err
}

// DestroyZvol destroys a zvol, optionally with all its snapshots
// (-r -R).
func (e *Exec) DestroyZvol(ctx context.Context, name string, recursive bool) error {
	args := []string{"destroy"}
	if recursive {
		args = append(args, "-r", "-R")
	}
	args = append(args, name)
	_, err := e.run(ctx, "zfs.DestroyZvol", e.ZfsPath, args...)
	return err
}

// Snapshot takes a snapshot of a dataset.
func (e *Exec) Snapshot(ctx context.Context, dataset, snap string) error {
	_, err := e.run(ctx, "zfs.Snapshot", e.ZfsPath, "snapshot", dataset+"@"+snap)
	return err
}

// DestroySnapshot destroys one snapshot.
func (e *Exec) DestroySnapshot(ctx context.Context, dataset, snap string) error {
	_, err := e.run(ctx, "zfs.DestroySnapshot", e.ZfsPath, "destroy", dataset+"@"+snap)
	return err
}

// ListSnapshots returns the snapshot names (not full dataset@snap paths)
// directly under dataset, oldest first as zfs lists them.
func (e *Exec) ListSnapshots(ctx context.Context, dataset string) ([]string, error) {
	out, err := e.run(ctx, "zfs.ListSnapshots", e.ZfsPath,
		"list", "-H", "-p", "-d", "1", "-t", "snapshot", "-o", "name", dataset)
	if err != nil {
		return nil, err
	}
	var snaps []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i := strings.IndexByte(line, '@'); i >= 0 {
			snaps = append(snaps, line[i+1:])
		}
	}
	return snaps, nil
}

// Send streams a full (from == "") or incremental (`-i from`) send stream
// of `to` to w. No temp files.
func (e *Exec) Send(ctx context.Context, dataset, from, to string, w io.Writer) error {
	args := []string{"send"}
	if from != "" {
		args = append(args, "-i", dataset+"@"+from)
	}
	args = append(args, dataset+"@"+to)
	return e.runWithIO(ctx, "zfs.Send", e.ZfsPath, nil, w, args...)
}

// Receive reads a send stream from r into dataset.
func (e *Exec) Receive(ctx context.Context, dataset string, r io.Reader) error {
	return e.runWithIO(ctx, "zfs.Receive", e.ZfsPath, r, nil, "receive", "-F", dataset)
}

// Resize grows (or shrinks) a zvol's volsize to size bytes.
func (e *Exec) Resize(ctx context.Context, zvol string, size uint64) error {
	_, err := e.run(ctx, "zfs.Resize", e.ZfsPath, "set",
		"volsize="+strconv.FormatUint(size, 10), zvol)
	return err
}

// Rollback reverts dataset to snap, destroying any snapshots taken
// after it (-r) — restore (G6.13) is a deliberate, destructive
// operation: the whole point is that anything newer than snap is gone.
func (e *Exec) Rollback(ctx context.Context, dataset, snap string) error {
	_, err := e.run(ctx, "zfs.Rollback", e.ZfsPath, "rollback", "-r", dataset+"@"+snap)
	return err
}

// PoolStatus parses `zpool status -H <pool>` into a PoolStatus.
func (e *Exec) PoolStatus(ctx context.Context, pool string) (*PoolStatus, error) {
	out, err := e.run(ctx, "zfs.PoolStatus", e.ZpoolPath, "status", "-H", pool)
	if err != nil {
		return nil, err
	}
	return parsePoolStatus(string(out)), nil
}

// Scrub starts a scrub on a pool (non-blocking; zpool scrub returns once
// the scrub is scheduled).
func (e *Exec) Scrub(ctx context.Context, pool string) error {
	_, err := e.run(ctx, "zfs.Scrub", e.ZpoolPath, "scrub", pool)
	return err
}

var validVdevStates = map[string]bool{
	"ONLINE": true, "DEGRADED": true, "FAULTED": true,
	"OFFLINE": true, "UNAVAIL": true, "REMOVED": true,
}

// parsePoolStatus handles `zpool status -H` output: tab-separated vdev
// rows (name, state, cksum/read/write columns), with a trailing
// non-tab-anchored "errors: ..." line.
func parsePoolStatus(out string) *PoolStatus {
	ps := &PoolStatus{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "errors:") {
			ps.Errors = parseErrorsLine(line)
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		name, state := strings.TrimSpace(fields[0]), strings.TrimSpace(fields[1])
		if !validVdevStates[state] {
			continue
		}
		if ps.Health == "" {
			ps.Health = state // first row is the pool itself
		}
		ps.Devices = append(ps.Devices, PoolDevice{Name: name, State: state})
	}
	return ps
}

// parseErrorsLine extracts a count from "errors: 12" or the common
// "errors: No known data errors" (0).
func parseErrorsLine(line string) uint64 {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "errors:"))
	if n, err := strconv.ParseUint(strings.Fields(rest)[0], 10, 64); err == nil {
		return n
	}
	return 0
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
