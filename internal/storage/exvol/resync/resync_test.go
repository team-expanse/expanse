package resync

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/expanse/expanse/internal/storage/zfs"
)

// fakeZFS records snapshot operations and fakes send streams.
type fakeZFS struct {
	mu     sync.Mutex
	snaps  map[string][]string // dataset → snapshot names
	sent   map[string][]byte   // "dataset|from|to" → stream payload
	logged []string
}

func newFakeZFS() *fakeZFS {
	return &fakeZFS{snaps: map[string][]string{}, sent: map[string][]byte{}}
}

func (f *fakeZFS) Snapshot(_ context.Context, dataset, snap string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snaps[dataset] = append(f.snaps[dataset], snap)
	return nil
}

func (f *fakeZFS) DestroySnapshot(_ context.Context, dataset, snap string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.snaps[dataset] {
		if s != snap {
			out = append(out, s)
		}
	}
	f.snaps[dataset] = out
	return nil
}

func (f *fakeZFS) ListSnapshots(_ context.Context, dataset string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.snaps[dataset]...)
	sort.Slice(out, func(i, j int) bool {
		a, _ := ParseSeq(out[i])
		b, _ := ParseSeq(out[j])
		return a < b
	})
	return out, nil
}

// Send fakes the zfs stream: incremental sends carry only the drift
// (bytes after the `from` marker); full sends carry everything.
func (f *fakeZFS) Send(_ context.Context, dataset, from, to string, w io.Writer) error {
	f.mu.Lock()
	payload := f.sent[dataset+"|"+from+"|"+to]
	f.mu.Unlock()
	_, err := w.Write(payload)
	return err
}

func (f *fakeZFS) Receive(_ context.Context, dataset string, r io.Reader) error {
	b, _ := io.ReadAll(r)
	f.mu.Lock()
	f.logged = append(f.logged, "recv:"+dataset+":"+strconv.Itoa(len(b)))
	f.mu.Unlock()
	return nil
}

// fakeSink is the remote replica's resync side.
type fakeSink struct {
	snaps []string
	mu    sync.Mutex
	got   bytes.Buffer
	adopt uint64
	full  bool
}

func (s *fakeSink) ListSnaps(context.Context) ([]string, error) { return s.snaps, nil }

func (s *fakeSink) Receive(_ context.Context, r io.Reader) (int64, error) {
	n, err := io.Copy(&s.got, r)
	return n, err
}

func (s *fakeSink) AdoptSeq(_ context.Context, seq uint64, full bool) error {
	s.adopt, s.full = seq, full
	return nil
}

// TestSnapshotRetention: the 11th snapshot evicts the oldest (G6.6).
func TestSnapshotRetention(t *testing.T) {
	ctx := context.Background()
	z := newFakeZFS()
	ds := "volumes/vol-x"
	for seq := uint64(1); seq <= 11; seq++ {
		if _, err := EnsureSnapshot(ctx, z, ds, seq, DefaultKeep); err != nil {
			t.Fatal(err)
		}
	}
	snaps, _ := z.ListSnapshots(ctx, ds)
	if len(snaps) != DefaultKeep {
		t.Fatalf("kept %d snapshots, want %d", len(snaps), DefaultKeep)
	}
	if snaps[0] != SnapName(2) {
		t.Fatalf("oldest kept = %s, want resync-2 (resync-1 evicted)", snaps[0])
	}
	for _, s := range snaps {
		if s == SnapName(1) {
			t.Fatal("resync-1 should have been destroyed")
		}
	}
}

// TestIncrementalVsFullSelection: common ancestor → incremental; no
// ancestor → full (WARNING).
func TestIncrementalVsFullSelection(t *testing.T) {
	ctx := context.Background()
	z := newFakeZFS()
	ds := "volumes/vol-y"

	// Primary holds resync-10..resync-30 (seq 30 newest); the stale
	// replica left at resync-10 → common ancestor → incremental.
	for seq := uint64(10); seq <= 30; seq += 10 {
		if _, err := EnsureSnapshot(ctx, z, ds, seq, 10); err != nil {
			t.Fatal(err)
		}
	}
	src, _ := z.ListSnapshots(ctx, ds)
	sink := &fakeSink{snaps: []string{SnapName(10)}}
	sinkDrift := []byte("DRIFT-BYTES")
	z.sent[ds+"|"+SnapName(10)+"|"+SnapName(30)] = sinkDrift

	res, err := Run(ctx, z, ds, src, sink, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if res.Full {
		t.Fatal("common ancestor exists — must be incremental")
	}
	if res.From != SnapName(10) || res.To != SnapName(30) {
		t.Fatalf("from/to = %s/%s, want resync-10/resync-30", res.From, res.To)
	}
	if res.Bytes != int64(len(sinkDrift)) {
		t.Fatalf("bytes = %d, want %d (drift only, not volume size)", res.Bytes, len(sinkDrift))
	}
	if sink.got.String() != string(sinkDrift) {
		t.Fatalf("sink got %q, want drift", sink.got.String())
	}
	if sink.adopt != 30 {
		t.Fatalf("adopt = %d, want 30", sink.adopt)
	}

	// No common ancestor → full send with a WARNING.
	sink2 := &fakeSink{snaps: nil}
	res2, err := Run(ctx, z, ds, src, sink2, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Full {
		t.Fatal("no ancestor — must be a full send")
	}
}

// TestRunRequiresSnapshot: no @resync-* snapshot → refused (the
// periodic snapshot requirement is the mechanism behind G6.6; without
// it every resync degrades to a full copy silently — not allowed).
func TestRunRequiresSnapshot(t *testing.T) {
	z := newFakeZFS()
	_, err := Run(context.Background(), z, "volumes/vol-z",
		nil, // no snapshots taken
		&fakeSink{}, slog.Default())
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("periodic snapshots")) {
		t.Fatalf("want periodic-snapshot error, got %v", err)
	}
}

// TestSnapNameRoundTrip.
func TestSnapNameRoundTrip(t *testing.T) {
	for _, seq := range []uint64{0, 1, 42, 1 << 40} {
		seq2, ok := ParseSeq(SnapName(seq))
		if !ok || seq2 != seq {
			t.Fatalf("round trip %d → %s → %d (ok=%v)", seq, SnapName(seq), seq2, ok)
		}
	}
	if _, ok := ParseSeq("autosnap-daily"); ok {
		t.Fatal("non-resync snapshots must not parse as resync seqs")
	}
}

// realSink adapts a real zvol as the resync target.
type realSink struct {
	z       ZFS
	dataset string
}

func (s *realSink) ListSnaps(ctx context.Context) ([]string, error) {
	return s.z.ListSnapshots(ctx, s.dataset)
}

func (s *realSink) Receive(ctx context.Context, r io.Reader) (int64, error) {
	if err := s.z.Receive(ctx, s.dataset, r); err != nil {
		return 0, err
	}
	return 0, nil
}

func (s *realSink) AdoptSeq(context.Context, uint64, bool) error { return nil }

// TestRealZvolIncrementalBytes (gated: EXPANSE_ZFS_TEST=1, pool
// `volumes` per nix/modules/storage-test.nix) verifies G6.6's
// proportionality claim on a real pool: an incremental send transfers
// bytes proportional to the DRIFT, not the volume size.
func TestRealZvolIncrementalBytes(t *testing.T) {
	if os.Getenv("EXPANSE_ZFS_TEST") != "1" {
		t.Skip("requires real ZFS pool (EXPANSE_ZFS_TEST=1)")
	}
	ctx := context.Background()
	z := zfs.New()
	pool := os.Getenv("EXPANSE_ZFS_POOL")
	if pool == "" {
		pool = "volumes"
	}
	src := pool + "/volumes/resync-test-src"
	tgt := pool + "/volumes/resync-test-tgt"
	_ = z.DestroyZvol(ctx, src, true)
	_ = z.DestroyZvol(ctx, tgt, true)
	for _, ds := range []string{src, tgt} {
		if err := z.CreateZvol(ctx, ds, 64<<20, nil); err != nil {
			t.Fatalf("create %s: %v", ds, err)
		}
		t.Cleanup(func() { _ = z.DestroyZvol(context.Background(), ds, true) })
	}
	// src: 16 MiB of data → snap resync-10 (the replica's base).
	dev := "/dev/zvol/" + src
	base := make([]byte, 16<<20)
	for i := range base {
		base[i] = byte(i)
	}
	if err := writeDev(dev, base); err != nil {
		t.Fatal(err)
	}
	if err := z.Snapshot(ctx, src, SnapName(10)); err != nil {
		t.Fatal(err)
	}
	// Seed the target with the base image (full send, once).
	pr, pw := io.Pipe()
	go func() { _ = z.Send(ctx, src, "", SnapName(10), pw); pw.Close() }()
	if err := z.Receive(ctx, tgt, pr); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	// src drifts 1 MiB → snap resync-20.
	drift := make([]byte, 1<<20)
	for i := range drift {
		drift[i] = byte(255 - i)
	}
	if err := writeDev(dev, drift); err != nil {
		t.Fatal(err)
	}
	if err := z.Snapshot(ctx, src, SnapName(20)); err != nil {
		t.Fatal(err)
	}
	srcSnaps, err := z.ListSnapshots(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	sink := &realSink{z: z, dataset: tgt}
	res, err := Run(ctx, z, src, srcSnaps, sink, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if res.Full {
		t.Fatal("common ancestor existed — resync must be incremental")
	}
	if res.Bytes > 8<<20 {
		t.Fatalf("incremental send moved %d bytes for 1 MiB of drift in a 64 MiB volume — not proportional to drift", res.Bytes)
	}
	if res.Bytes == 0 {
		t.Fatal("incremental send transferred nothing")
	}
}

// writeDev writes the whole buffer to a zvol device (buffered path —
// correctness of content is what matters here, not O_DIRECT).
func writeDev(dev string, b []byte) error {
	f, err := os.OpenFile(dev, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(b)
	return err
}

// TestRunNewestSnapshotNumeric: the source snapshot to send is chosen
// by parsed seq, not zfs list order — at seq >= 10 lexicographic order
// puts resync-4 AFTER resync-10, and "last element" would send the
// wrong (older) snapshot.
func TestRunNewestSnapshotNumeric(t *testing.T) {
	z := newFakeZFS()
	snaps := []string{"resync-2", "resync-10", "resync-4"} // lexicographic order
	sink := &fakeSink{}
	res, err := Run(context.Background(), z, "volumes/vol-order", snaps, sink, slog.Default())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.To != "resync-10" {
		t.Fatalf("sent %q, want resync-10", res.To)
	}
	if res.Adopt != 10 {
		t.Fatalf("adopted seq %d, want 10", res.Adopt)
	}
}

// TestRunDegenerateSameNameSnapshot: a target claiming the SAME newest
// snapshot name as the source is not proof of shared generation — two
// independent primaries can snapshot the same seq independently (VM
// run 8), and `zfs send -i x x` fails on real ZFS ("not an earlier
// snapshot from the same fs"), leaving the replica stale forever. Run
// must degrade to a FULL send in that case.
func TestRunDegenerateSameNameSnapshot(t *testing.T) {
	z := newFakeZFS()
	src := []string{"resync-0"}
	sink := &fakeSink{snaps: []string{"resync-0"}}
	z.sent["volumes/vol-same|"+"|resync-0"] = []byte("FULL")

	res, err := Run(context.Background(), z, "volumes/vol-same", src, sink, slog.Default())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Full || res.From != "" {
		t.Fatalf("res = %+v, want degenerate case forced to full send", res)
	}
}
