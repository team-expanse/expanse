// Package resync is the §4.3 resync mechanism (Phase 06 T12): periodic
// `@resync-<seq>` snapshots on the primary (retained, last 10) are what
// make incremental replica rebuilds possible — G6.6. When a Stale
// replica rejoins, the newest COMMON ANCESTOR snapshot seeds a
// `zfs send -i` incremental stream over the mesh; with no common
// ancestor the stream is a full send (logged as a WARNING — it should
// be rare; if it isn't, that's a bug to flag, not silence).
//
// Resync traffic is rate-limited (default 100 MiB/s, token bucket) and
// DSCP-marked (CS1) so it never starves foreground I/O; ongoing writes
// continue against the remaining quorum during the send.
package resync

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/expanse/expanse/internal/storage/zfs"
)

// SnapPrefix is the snapshot-name prefix (`@resync-<seq>`); the seq is
// the primary's last assigned sequence when the snapshot was taken.
const SnapPrefix = "resync-"

// DefaultKeep is the snapshot retention (spec: retain the last 10).
const DefaultKeep = 10

// SnapName is the snapshot name for a primary sequence.
func SnapName(seq uint64) string { return SnapPrefix + strconv.FormatUint(seq, 10) }

// ParseSeq recovers the sequence from a snapshot name.
func ParseSeq(name string) (uint64, bool) {
	s, ok := strings.CutPrefix(name, SnapPrefix)
	if !ok {
		return 0, false
	}
	seq, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return seq, true
}

// ZFS is the snapshot/send/recv surface (*zfs.Exec satisfies it; fakes
// in tests).
type ZFS interface {
	Snapshot(ctx context.Context, dataset, snap string) error
	DestroySnapshot(ctx context.Context, dataset, snap string) error
	ListSnapshots(ctx context.Context, dataset string) ([]string, error)
	Send(ctx context.Context, dataset, from, to string, w io.Writer) error
	Receive(ctx context.Context, dataset string, r io.Reader) error
}

// Retain keeps the newest keep snapshots of dataset, destroying older
// ones (the 11th snapshot evicts the oldest). Returns the destroyed
// names.
func Retain(ctx context.Context, z ZFS, dataset string, keep int) ([]string, error) {
	if keep <= 0 {
		keep = DefaultKeep
	}
	snaps, err := z.ListSnapshots(ctx, dataset)
	if err != nil {
		return nil, err
	}
	var resyncSnaps []string
	for _, s := range snaps {
		if _, ok := ParseSeq(s); ok {
			resyncSnaps = append(resyncSnaps, s)
		}
	}
	// Names sort lexicographically; resync-<seq> is zero-padded by
	// seq width, so sort numerically by parsed seq.
	sort.Slice(resyncSnaps, func(i, j int) bool {
		a, _ := ParseSeq(resyncSnaps[i])
		b, _ := ParseSeq(resyncSnaps[j])
		return a < b
	})
	var destroyed []string
	for len(resyncSnaps) > keep {
		oldest := resyncSnaps[0]
		resyncSnaps = resyncSnaps[1:]
		if err := z.DestroySnapshot(ctx, dataset, oldest); err != nil {
			return destroyed, err
		}
		destroyed = append(destroyed, oldest)
	}
	return destroyed, nil
}

// CommonAncestor returns the newest snapshot present on both sides.
func CommonAncestor(source, target []string) (string, bool) {
	tset := map[string]bool{}
	for _, s := range target {
		tset[s] = true
	}
	var best string
	found := false
	bestSeq := uint64(0)
	for _, s := range source {
		seq, ok := ParseSeq(s)
		if !ok || !tset[s] {
			continue
		}
		if !found || seq > bestSeq {
			best, bestSeq, found = s, seq, true
		}
	}
	return best, found
}

// Result reports one resync run.
type Result struct {
	From  string // incremental base ("" = full send)
	To    string // snapshot streamed
	Full  bool   // true → no common ancestor was found (WARNING case)
	Bytes int64  // stream bytes transferred
	Adopt uint64 // seq the target adopted
}

// TargetSink is the remote replica's resync side: snapshot listing
// (common-ancestor discovery), stream receive, and seq adoption.
type TargetSink interface {
	// ListSnaps returns the target's local snapshot names.
	ListSnaps(ctx context.Context) ([]string, error)
	// Receive streams the zfs send stream into the replica.
	Receive(ctx context.Context, r io.Reader) (int64, error)
	// AdoptSeq latches the replica's post-resync sequence (step 5).
	AdoptSeq(ctx context.Context, seq uint64, full bool) error
}

// Run performs one resync of dataset toward target (§4.3 resync steps
// 1–5). sourceSnaps must already contain a current `@resync-<seq>`
// snapshot (the primary takes it before calling). Ongoing writes are
// untouched: the send reads a snapshot, and the primary keeps quorum
// with its healthy replicas during the stream.
func Run(ctx context.Context, z ZFS, dataset string, sourceSnaps []string, sink TargetSink, log *slog.Logger) (Result, error) {
	var res Result
	if len(sourceSnaps) == 0 {
		return res, fmt.Errorf("resync: primary has no @resync-* snapshot to send (G6.6 requires periodic snapshots)")
	}
	// Newest source snapshot is the resync target (steps 1–2). Select
	// by PARSED seq, not list order: zfs list returns names
	// lexicographically, and resync-<seq> is not zero-padded — at
	// seq >= 10, resync-4 sorts after resync-10 and "last element"
	// would send the WRONG (older) snapshot.
	to := sourceSnaps[0]
	toSeq, _ := ParseSeq(to)
	for _, s := range sourceSnaps[1:] {
		if q, ok := ParseSeq(s); ok && q > toSeq {
			to, toSeq = s, q
		}
	}
	res.To = to

	targetSnaps, err := sink.ListSnaps(ctx) // step 1 (remote)
	if err != nil {
		return res, fmt.Errorf("resync: list target snapshots: %w", err)
	}
	from, common := CommonAncestor(sourceSnaps, targetSnaps)
	if common {
		res.From = from
	} else {
		res.Full = true
		if log != nil {
			// Spec-required WARNING: a full send means the replica
			// drifted past every retained snapshot — rare by design;
			// if this is common, snapshot cadence or retention is wrong.
			log.Warn("resync: no common ancestor snapshot — FULL send (should be rare; investigate if frequent)",
				"dataset", dataset, "target_snapshots", len(targetSnaps))
		}
	}

	// Steps 3–4: stream `zfs send [-i from] to` into the sink. The
	// sink side rate-limits and DSCP-marks the connection.
	pr, pw := io.Pipe()
	errC := make(chan error, 1)
	go func() {
		errC <- z.Send(ctx, dataset, from, to, pw)
		pw.Close()
	}()
	n, rerr := sink.Receive(ctx, pr)
	res.Bytes = n
	if serr := <-errC; serr != nil {
		return res, fmt.Errorf("resync: zfs send: %w", serr)
	}
	if rerr != nil {
		return res, fmt.Errorf("resync: replica receive: %w", rerr)
	}

	// Step 5: the replica reaches the current seq and rejoins quorum.
	if err := sink.AdoptSeq(ctx, toSeq, res.Full); err != nil {
		return res, fmt.Errorf("resync: adopt seq %d: %w", toSeq, err)
	}
	res.Adopt = toSeq
	return res, nil
}

// EnsureSnapshot takes @resync-<seq> if not already present, then
// enforces retention (the periodic-snapshot requirement behind G6.6).
func EnsureSnapshot(ctx context.Context, z ZFS, dataset string, seq uint64, keep int) (string, error) {
	name := SnapName(seq)
	existing, err := z.ListSnapshots(ctx, dataset)
	if err != nil {
		return "", err
	}
	for _, s := range existing {
		if s == name {
			return name, nil
		}
	}
	if err := z.Snapshot(ctx, dataset, name); err != nil {
		return "", err
	}
	if _, err := Retain(ctx, z, dataset, keep); err != nil {
		return name, err
	}
	return name, nil
}

// execZFS adapts *zfs.Exec to ZFS (compile-time check).
var _ ZFS = (*zfs.Exec)(nil)
