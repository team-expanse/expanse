package raftstore

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/expanse/expanse/internal/cluster/generation"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

// testLog wraps bytes in a raft.Log with an arbitrary index (the FSM does
// not use the index).
func testLog(data []byte) *raft.Log {
	return &raft.Log{Data: data, Index: 1, Type: raft.LogCommand}
}

func applyOK(t *testing.T, f *FSM, c *pb.Command) store.Revision {
	t.Helper()
	c.Version = CommandVersion
	b, err := encodeCommand(c)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	res := f.Apply(testLog(b))
	ar, ok := res.(*applyResult)
	if !ok {
		t.Fatalf("Apply returned %T, want *applyResult", res)
	}
	if ar.err != nil {
		t.Fatalf("Apply(%s %s): %v", c.GetType(), c.GetKey(), ar.err)
	}
	return ar.rev
}

func applyErr(t *testing.T, f *FSM, c *pb.Command) error {
	t.Helper()
	c.Version = CommandVersion
	b, err := encodeCommand(c)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	res := f.Apply(testLog(b))
	ar, ok := res.(*applyResult)
	if !ok {
		t.Fatalf("Apply returned %T, want *applyResult", res)
	}
	return ar.err
}

func TestFSMPutGetDelete(t *testing.T) {
	f := NewFSM()
	rev := applyOK(t, f, &pb.Command{Type: pb.CommandType_COMMAND_TYPE_PUT, Key: "/foo", Value: []byte("bar"), TimestampUnixNs: 100})
	if rev != 1 {
		t.Errorf("rev = %d, want 1", rev)
	}
	e, err := f.get("/foo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(e.Value) != "bar" || e.CreatedAt != 100 || e.UpdatedAt != 100 || e.Revision != 1 {
		t.Errorf("entry = %+v", e)
	}
	// Overwrite keeps CreatedAt. An unconditional Put is encoded as a
	// single-op Txn (CmdPut with Expect=0 means "must not exist").
	applyOK(t, f, &pb.Command{
		Type: pb.CommandType_COMMAND_TYPE_TXN, TimestampUnixNs: 200,
		Ops: []*pb.Op{{Kind: pb.OpKind_OP_KIND_PUT, Key: "/foo", Value: []byte("baz")}},
	})
	e, _ = f.get("/foo")
	if string(e.Value) != "baz" || e.CreatedAt != 100 || e.UpdatedAt != 200 {
		t.Errorf("entry after overwrite = %+v", e)
	}
	applyOK(t, f, &pb.Command{Type: pb.CommandType_COMMAND_TYPE_DELETE, Key: "/foo"})
	if _, err := f.get("/foo"); !errors.Is(err, errors.KindNotFound) {
		t.Errorf("get after delete: kind = %q", errors.KindOf(err))
	}
}

func TestFSMCASPreconditions(t *testing.T) {
	f := NewFSM()
	// expect=0 on missing key: create.
	applyOK(t, f, &pb.Command{Type: pb.CommandType_COMMAND_TYPE_PUT, Key: "/k", Value: []byte("v1"), Expect: 0, TimestampUnixNs: 1})
	// expect=0 on existing key: conflict.
	if err := applyErr(t, f, &pb.Command{Type: pb.CommandType_COMMAND_TYPE_PUT, Key: "/k", Value: []byte("v2"), Expect: 0}); !errors.Is(err, errors.KindConflict) {
		t.Errorf("CAS create on existing: kind = %q", errors.KindOf(err))
	}
	// non-zero expect on missing key: conflict.
	if err := applyErr(t, f, &pb.Command{Type: pb.CommandType_COMMAND_TYPE_PUT, Key: "/no", Value: []byte("v"), Expect: 5}); !errors.Is(err, errors.KindConflict) {
		t.Errorf("CAS on missing: kind = %q", errors.KindOf(err))
	}
	// stale expect: conflict, value unchanged.
	if err := applyErr(t, f, &pb.Command{Type: pb.CommandType_COMMAND_TYPE_PUT, Key: "/k", Value: []byte("v3"), Expect: 99}); !errors.Is(err, errors.KindConflict) {
		t.Errorf("stale CAS: kind = %q", errors.KindOf(err))
	}
	e, _ := f.get("/k")
	if string(e.Value) != "v1" {
		t.Errorf("value changed on failed CAS: %q", e.Value)
	}
	// matching expect: success.
	if err := applyErr(t, f, &pb.Command{Type: pb.CommandType_COMMAND_TYPE_PUT, Key: "/k", Value: []byte("v3"), Expect: 1}); err != nil {
		t.Fatalf("matching CAS: %v", err)
	}
	// delete preconditions.
	if err := applyErr(t, f, &pb.Command{Type: pb.CommandType_COMMAND_TYPE_DELETE, Key: "/missing"}); !errors.Is(err, errors.KindNotFound) {
		t.Errorf("delete missing: kind = %q", errors.KindOf(err))
	}
	if err := applyErr(t, f, &pb.Command{Type: pb.CommandType_COMMAND_TYPE_DELETE, Key: "/k", Expect: 1}); !errors.Is(err, errors.KindConflict) {
		t.Errorf("stale delete: kind = %q", errors.KindOf(err))
	}
}

func TestFSMTxnAtomicity(t *testing.T) {
	f := NewFSM()
	applyOK(t, f, &pb.Command{Type: pb.CommandType_COMMAND_TYPE_PUT, Key: "/a", Value: []byte("1"), TimestampUnixNs: 1})
	applyOK(t, f, &pb.Command{Type: pb.CommandType_COMMAND_TYPE_PUT, Key: "/b", Value: []byte("x"), TimestampUnixNs: 1})

	// Failing txn: nothing applied.
	bad := &pb.Command{
		Type: pb.CommandType_COMMAND_TYPE_TXN,
		Ops: []*pb.Op{
			{Kind: pb.OpKind_OP_KIND_PUT, Key: "/a", Value: []byte("2")},
			{Kind: pb.OpKind_OP_KIND_CHECK, Key: "/b", Expect: 999},
		},
	}
	if err := applyErr(t, f, bad); !errors.Is(err, errors.KindConflict) {
		t.Fatalf("failing txn: kind = %q", errors.KindOf(err))
	}
	e, _ := f.get("/a")
	if string(e.Value) != "1" {
		t.Error("partial txn applied")
	}

	// Succeeding txn: one revision for all puts.
	good := &pb.Command{
		Type: pb.CommandType_COMMAND_TYPE_TXN,
		Ops: []*pb.Op{
			{Kind: pb.OpKind_OP_KIND_CHECK, Key: "/b", Expect: 2},
			{Kind: pb.OpKind_OP_KIND_PUT, Key: "/x", Value: []byte("1")},
			{Kind: pb.OpKind_OP_KIND_PUT, Key: "/y", Value: []byte("2")},
			{Kind: pb.OpKind_OP_KIND_DELETE, Key: "/b"},
		},
		TimestampUnixNs: 5,
	}
	rev := applyOK(t, f, good)
	e1, _ := f.get("/x")
	e2, _ := f.get("/y")
	if e1.Revision != rev || e2.Revision != rev {
		t.Errorf("txn revisions %d, %d != %d", e1.Revision, e2.Revision, rev)
	}
	if _, err := f.get("/b"); !errors.Is(err, errors.KindNotFound) {
		t.Error("txn delete not applied")
	}
	// Txn delete of missing key: not found, nothing applied.
	if err := applyErr(t, f, &pb.Command{
		Type: pb.CommandType_COMMAND_TYPE_TXN,
		Ops:  []*pb.Op{{Kind: pb.OpKind_OP_KIND_DELETE, Key: "/nope"}},
	}); !errors.Is(err, errors.KindNotFound) {
		t.Errorf("txn delete missing: kind = %q", errors.KindOf(err))
	}
}

func TestFSMIdempotency(t *testing.T) {
	f := NewFSM()
	c := &pb.Command{Type: pb.CommandType_COMMAND_TYPE_PUT, Key: "/k", Value: []byte("v"), TimestampUnixNs: 1, RequestId: "req-42"}
	r1 := applyOK(t, f, c)
	// Same RequestID retried: replays the result, does not bump revision.
	r2 := applyOK(t, f, c)
	if r1 != r2 {
		t.Errorf("retry revision %d != original %d", r2, r1)
	}
	if f.Revision() != r1 {
		t.Errorf("fsm revision = %d, want %d", f.Revision(), r1)
	}
	// A retried FAILED request replays as failure.
	bad := &pb.Command{Type: pb.CommandType_COMMAND_TYPE_PUT, Key: "/k", Value: []byte("v"), Expect: 0, RequestId: "req-43"}
	if err := applyErr(t, f, bad); !errors.Is(err, errors.KindConflict) {
		t.Fatalf("original failed apply: kind = %q", errors.KindOf(err))
	}
	if err := applyErr(t, f, bad); !errors.Is(err, errors.KindConflict) {
		t.Errorf("retried failed apply: kind = %q", errors.KindOf(err))
	}
	// Same request ID with a DIFFERENT command replays the original result
	// (idempotency key wins — clients must never reuse IDs).
	other := &pb.Command{Type: pb.CommandType_COMMAND_TYPE_PUT, Key: "/other", Value: []byte("x"), TimestampUnixNs: 2, RequestId: "req-42"}
	r3 := applyOK(t, f, other)
	if r3 != r1 {
		t.Errorf("different command with same ID: rev %d, want replay of %d", r3, r1)
	}
	if _, err := f.get("/other"); !errors.Is(err, errors.KindNotFound) {
		t.Error("duplicate-ID command must not be applied")
	}
}

func TestFSMListSorted(t *testing.T) {
	f := NewFSM()
	for _, k := range []string{"/b", "/a/2", "/a/1", "/ab", "/c"} {
		applyOK(t, f, &pb.Command{Type: pb.CommandType_COMMAND_TYPE_PUT, Key: k, Value: []byte("v"), TimestampUnixNs: 1})
	}
	entries, err := f.list("/a/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || string(entries[0].Key) != "/a/1" || string(entries[1].Key) != "/a/2" {
		t.Errorf("list = %v", entries)
	}
	all, _ := f.list("/")
	if len(all) != 5 {
		t.Errorf("len(all) = %d, want 5", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].Key >= all[i].Key {
			t.Error("list not sorted")
		}
	}
}

func TestFSMWatchers(t *testing.T) {
	f := NewFSM()
	chAll := f.watchers.watch(t.Context(), "/", 0)
	chA := f.watchers.watch(t.Context(), "/a/", 0)

	applyOK(t, f, &pb.Command{Type: pb.CommandType_COMMAND_TYPE_PUT, Key: "/a/1", Value: []byte("1"), TimestampUnixNs: 1})
	applyOK(t, f, &pb.Command{Type: pb.CommandType_COMMAND_TYPE_PUT, Key: "/b", Value: []byte("2"), TimestampUnixNs: 1})

	ev := <-chAll
	if string(ev.Entry.Key) != "/a/1" {
		t.Errorf("first event = %s, want /a/1", ev.Entry.Key)
	}
	ev = <-chA
	if string(ev.Entry.Key) != "/a/1" {
		t.Errorf("filtered event = %s, want /a/1", ev.Entry.Key)
	}
	select {
	case ev := <-chA:
		t.Errorf("unexpected event on /a/ watcher: %s", ev.Entry.Key)
	default:
	}
	// fromRev excludes earlier events.
	revNow := f.Revision()
	chLate := f.watchers.watch(t.Context(), "/", revNow)
	applyOK(t, f, &pb.Command{Type: pb.CommandType_COMMAND_TYPE_PUT, Key: "/c", Value: []byte("3"), TimestampUnixNs: 1})
	ev = <-chLate
	if string(ev.Entry.Key) != "/c" {
		t.Errorf("late watcher first event = %s, want /c", ev.Entry.Key)
	}
}

// TestFSMSnapshotRestore round-trips a large state through the canonical
// serializer (spec: 100k keys).
func TestFSMSnapshotRestore(t *testing.T) {
	if testing.Short() {
		t.Skip("100k-key snapshot skipped in short mode")
	}
	f := NewFSM()
	const n = 100_000
	for i := 0; i < n; i++ {
		c := &pb.Command{
			Type:            pb.CommandType_COMMAND_TYPE_PUT,
			Key:             fmt.Sprintf("/keys/%08d", i),
			Value:           []byte(fmt.Sprintf("value-%d", i%97)),
			TimestampUnixNs: int64(i + 1),
		}
		c.Version = CommandVersion
		b, _ := encodeCommand(c)
		if ar := f.Apply(testLog(b)); ar.(*applyResult).err != nil {
			t.Fatalf("apply %d: %v", i, ar.(*applyResult).err)
		}
	}

	snap, err := f.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// Persist into memory.
	var sink bytes.Buffer
	if err := snap.(*fsmSnapshot).Persist(&memSink{&sink}); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	f2 := NewFSM()
	if err := f2.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if f2.Revision() != f.Revision() {
		t.Errorf("revision %d != %d", f2.Revision(), f.Revision())
	}
	h1, h2 := f.StateHash(), f2.StateHash()
	if h1 != h2 {
		t.Error("state hashes differ after restore")
	}
	entries, _ := f2.list("/keys/")
	if len(entries) != n {
		t.Errorf("len = %d, want %d", len(entries), n)
	}
}

type memSink struct{ b *bytes.Buffer }

func (m *memSink) Write(p []byte) (int, error) { return m.b.Write(p) }
func (m *memSink) Close() error                { return nil }
func (m *memSink) ID() string                  { return "test" }
func (m *memSink) Cancel() error               { return nil }

func TestFSMRestoreRejectsGarbage(t *testing.T) {
	f := NewFSM()
	for _, bad := range [][]byte{
		{},
		{0x01},
		// absurd key length
		append(u64be(0), append(u64be(1), append(u64be(1<<30), 0x00)...)...),
	} {
		if err := f.Restore(io.NopCloser(bytes.NewReader(bad))); err == nil {
			t.Errorf("Restore accepted %v", bad)
		}
	}
	// Trailing bytes rejected.
	good := f.canonicalSnapshot()
	if err := f.Restore(io.NopCloser(bytes.NewReader(append(good, 0x00)))); err == nil {
		t.Error("Restore accepted trailing bytes")
	}
}

func u64be(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}

// genDeterministicLog generates a fixed pseudo-random log. It uses its own
// xorshift PRNG with a fixed seed so the log (and therefore the resulting
// state) is identical everywhere — never use math/rand's global source in
// the FSM itself.
func genDeterministicLog(n int) [][]byte {
	type rng struct{ s uint64 }
	next := func(r *rng) uint64 {
		r.s ^= r.s << 13
		r.s ^= r.s >> 7
		r.s ^= r.s << 17
		return r.s
	}
	r := &rng{s: 0x9E3779B97F4A7C15}
	log := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		choice := next(r) % 10
		key := fmt.Sprintf("/keys/%04d", next(r)%500)
		ts := int64(i + 1)
		var c pb.Command
		c.Version = CommandVersion
		c.TimestampUnixNs = ts
		switch {
		case choice < 6: // put (some with CAS expect)
			c.Type = pb.CommandType_COMMAND_TYPE_PUT
			c.Key = key
			c.Value = []byte(fmt.Sprintf("v%d", next(r)%1000))
			if next(r)%3 == 0 {
				c.Expect = next(r) % 10 // may fail preconditions — fine
			}
			if next(r)%2 == 0 {
				c.RequestId = fmt.Sprintf("req-%d", next(r)%2000)
			}
		case choice < 8: // delete
			c.Type = pb.CommandType_COMMAND_TYPE_DELETE
			c.Key = key
			if next(r)%3 == 0 {
				c.Expect = next(r) % 10
			}
		default: // txn
			c.Type = pb.CommandType_COMMAND_TYPE_TXN
			c.Ops = []*pb.Op{
				{Kind: pb.OpKind_OP_KIND_CHECK, Key: key, Expect: next(r) % 10},
				{Kind: pb.OpKind_OP_KIND_PUT, Key: fmt.Sprintf("/txn/%04d", next(r)%300), Value: []byte("t")},
			}
		}
		b, err := encodeCommand(&c)
		if err != nil {
			panic(err)
		}
		log = append(log, b)
	}
	return log
}

// TestFSMDeterminism replays a fixed 10,000-entry log on three fresh FSMs
// and asserts byte-identical serialized state (spec §4.1 determinism rules).
func TestFSMDeterminism(t *testing.T) {
	const n = 10_000
	log := genDeterministicLog(n)
	hashes := make([][32]byte, 3)
	for i := range hashes {
		f := NewFSM()
		for j, data := range log {
			res := f.Apply(&raft.Log{Data: data, Index: uint64(j + 1), Type: raft.LogCommand})
			if ar, ok := res.(*applyResult); ok && ar.err != nil {
				// Application errors are fine (preconditions); they must
				// just be deterministic — checked via the hash below.
				_ = ar.err
			}
		}
		hashes[i] = f.StateHash()
	}
	if hashes[0] != hashes[1] || hashes[1] != hashes[2] {
		t.Fatal("FSM divergence: identical log produced different state on 3 replicas")
	}
	// And the state must be non-trivial.
	f := NewFSM()
	for j, data := range log {
		f.Apply(&raft.Log{Data: data, Index: uint64(j + 1), Type: raft.LogCommand})
	}
	if entries, _ := f.list("/"); len(entries) < 100 {
		t.Errorf("determinism log produced only %d entries — test is too weak", len(entries))
	}
}

// assertDeterministic re-applies a log twice and compares hashes.
func assertDeterministic(t *testing.T, log [][]byte) {
	t.Helper()
	run := func() [32]byte {
		f := NewFSM()
		for j, data := range log {
			f.Apply(&raft.Log{Data: data, Index: uint64(j + 1), Type: raft.LogCommand})
		}
		return f.StateHash()
	}
	if first, second := run(), run(); first != second {
		t.Error("same log, different hashes across two runs")
	}
}

func TestAssertDeterministicHelper(t *testing.T) {
	assertDeterministic(t, genDeterministicLog(500))
}

// TestFSMDeterminismBadTimestamp ensures the FSM never reads a wall clock:
// entries with identical content and different wall-clock apply times
// produce identical state. (The FSM has no clock access at all — this test
// documents the invariant; the real enforcement is code review + the
// determinism test above.)
func TestFSMDeterminismBadTimestamp(t *testing.T) {
	log := genDeterministicLog(300)
	assertDeterministic(t, log)
	// Sanity: randomized garbage values must never crash Apply.
	f := NewFSM()
	b := make([]byte, 64)
	for i := 0; i < 100; i++ {
		rand.Read(b)
		f.Apply(testLog(b)) // must not panic
	}
}

// TestFSMRetentionVictims verifies the retention rule (§4.7): keep the
// newest KeepLast generations plus everything created within the last
// 30 days; everything else is pruned. Uses fake meta timestamps — the
// victim calculation must be deterministic regardless of wall clock.
func TestFSMRetentionVictims(t *testing.T) {
	f := NewFSM()
	now := time.Now().UnixNano()
	old := now - int64(40*24*time.Hour) // 40 days ago

	writeMeta := func(n uint64, ts int64) {
		meta, _ := json.Marshal(generation.Generation{
			Number: n, CreatedAt: time.Unix(0, ts).UTC(),
		})
		f.data[generation.MetaKey(n)] = &store.Entry{Key: generation.MetaKey(n), Value: meta}
		f.data[generation.DataKey(n)] = &store.Entry{Key: generation.DataKey(n), Value: []byte("{}")}
	}

	// Gens 1–60: created 40 days ago (old).
	for n := uint64(1); n <= 60; n++ {
		writeMeta(n, old)
	}
	// Gens 61–70: fresh.
	for n := uint64(61); n <= 70; n++ {
		writeMeta(n, now)
	}

	victims := map[uint64]bool{}
	for _, v := range f.retentionVictimsLocked(70, now) {
		victims[v] = true
	}
	// Newest 50 (21..70) kept; fresh ones (61..70) kept regardless.
	// Old beyond the newest 50: 1..20 pruned.
	for n := uint64(1); n <= 20; n++ {
		if !victims[n] {
			t.Errorf("gen %d should be pruned", n)
		}
	}
	for n := uint64(21); n <= 70; n++ {
		if victims[n] {
			t.Errorf("gen %d should be retained", n)
		}
	}
}
