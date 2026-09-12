package generation

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

func TestIsDesiredKey(t *testing.T) {
	t.Parallel()
	desired := []string{
		"/blocks/web", "/volumes/db", "/networks/mesh", "/cluster/config/quorum",
	}
	for _, k := range desired {
		if !IsDesiredKey(store.Key(k)) {
			t.Errorf("IsDesiredKey(%q) = false, want true", k)
		}
	}
	not := []string{
		"/nodes/n1/status", "/blocks/web/status", "/volumes/db/status",
		"/cluster/config/lb/status", "/events/123", "/cluster/meta",
		"/generations/2/data", "/cluster/generation",
	}
	for _, k := range not {
		if IsDesiredKey(store.Key(k)) {
			t.Errorf("IsDesiredKey(%q) = true, want false", k)
		}
	}
}

// TestHashStableUnderMapShuffle: canonical serialization must not depend
// on Go map iteration order (FSM determinism rule 2).
func TestHashStableUnderMapShuffle(t *testing.T) {
	t.Parallel()
	build := func() map[store.Key][]byte {
		return map[store.Key][]byte{
			store.Key("/blocks/a"):   []byte("1"),
			store.Key("/blocks/b"):   []byte("2"),
			store.Key("/volumes/c"):  []byte("3"),
			store.Key("/networks/d"): []byte("4"),
		}
	}
	h1 := Hash(build())
	h2 := Hash(build())
	if h1 != h2 {
		t.Errorf("hash unstable: %s != %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("hash length = %d, want 64 (sha256 hex)", len(h1))
	}

	// Content change → different hash.
	m := build()
	m[store.Key("/blocks/a")] = []byte("9")
	if Hash(m) == h1 {
		t.Error("changed content produced identical hash")
	}

	// Key order in the encoding is sorted.
	var encParsed Snapshot
	if err := json.Unmarshal(EncodeSnapshot(build()), &encParsed); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if len(encParsed.Keys) == 0 || encParsed.Keys[0].Key != "/blocks/a" {
		t.Errorf("first key = %+v, want /blocks/a first", encParsed.Keys)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	t.Parallel()
	m := map[store.Key][]byte{
		store.Key("/blocks/a"): []byte(`{"x":1}`),
		store.Key("/blocks/b"): nil,
	}
	dec, err := DecodeSnapshot(EncodeSnapshot(m))
	if err != nil {
		t.Fatalf("DecodeSnapshot: %v", err)
	}
	if len(dec) != 2 || string(dec[store.Key("/blocks/a")]) != `{"x":1}` {
		t.Errorf("round-trip = %v", dec)
	}
	if _, err := DecodeSnapshot([]byte("not json")); err == nil {
		t.Error("garbage accepted")
	}
}

// boltAt opens a boltstore in a temp dir for client-side op tests.
func boltAt(t *testing.T) store.Store {
	t.Helper()
	st, err := boltstore.New(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("boltstore.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// writeGen fabricates generation n in the store (boltstore has no FSM
// hook — this simulates what the raftstore FSM does on mutation).
func writeGen(t *testing.T, ctx context.Context, st store.Store, n uint64, m map[store.Key][]byte) {
	t.Helper()
	meta, err := json.Marshal(Generation{
		Number: n, CreatedAt: time.Unix(0, int64(n)*1e9).UTC(),
		CreatedBy: "system", Hash: Hash(m),
	})
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	for _, op := range []store.Op{
		{Kind: store.OpPut, Key: DataKey(n), Value: EncodeSnapshot(m)},
		{Kind: store.OpPut, Key: MetaKey(n), Value: meta},
		{Kind: store.OpPut, Key: CurrentKey, Value: []byte(strconv.FormatUint(n, 10))},
	} {
		if _, err := st.Txn(ctx, []store.Op{op}); err != nil {
			t.Fatalf("writeGen %d: %v", n, err)
		}
	}
}

func TestDiffAndListClientSide(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := boltAt(t)

	writeGen(t, ctx, st, 1, map[store.Key][]byte{
		store.Key("/blocks/a"): []byte("1"),
		store.Key("/blocks/b"): []byte("2"),
	})
	writeGen(t, ctx, st, 4, map[store.Key][]byte{
		store.Key("/blocks/a"): []byte("1b"),
		store.Key("/blocks/c"): []byte("3"),
	})

	d, err := DiffGenerations(ctx, st, 1, 4)
	if err != nil {
		t.Fatalf("DiffGenerations: %v", err)
	}
	if len(d.Added) != 1 || d.Added[0] != "/blocks/c" {
		t.Errorf("added = %v", d.Added)
	}
	if len(d.Changed) != 1 || d.Changed[0] != "/blocks/a" {
		t.Errorf("changed = %v", d.Changed)
	}
	if len(d.Removed) != 1 || d.Removed[0] != "/blocks/b" {
		t.Errorf("removed = %v", d.Removed)
	}

	gens, err := List(ctx, st)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(gens) != 2 {
		t.Fatalf("List = %d generations, want 2", len(gens))
	}
	if gens[0].Number != 1 || gens[1].Number != 4 {
		t.Errorf("gens = %d,%d; want 1,4 (sorted)", gens[0].Number, gens[1].Number)
	}

	cur, err := Current(ctx, st)
	if err != nil || cur != 4 {
		t.Errorf("Current = %d, %v; want 4", cur, err)
	}
}

// TestRollbackClientSide exercises Rollback against a boltstore with
// hand-written generation history (the append-only semantics are the
// package's own; the FSM integration is tested in raftstore).
func TestRollbackClientSide(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := boltAt(t)

	writeGen(t, ctx, st, 1, map[store.Key][]byte{
		store.Key("/blocks/a"): []byte("one"),
	})
	writeGen(t, ctx, st, 2, map[store.Key][]byte{
		store.Key("/blocks/a"): []byte("one"),
		store.Key("/blocks/b"): []byte("two"),
	})

	// Sync the live desired state to generation 2, then roll back to 1.
	for _, op := range []store.Op{
		{Kind: store.OpPut, Key: store.Key("/blocks/a"), Value: []byte("one")},
		{Kind: store.OpPut, Key: store.Key("/blocks/b"), Value: []byte("two")},
	} {
		if _, err := st.Txn(ctx, []store.Op{op}); err != nil {
			t.Fatalf("seed state: %v", err)
		}
	}
	n, err := Rollback(ctx, st, 0, "user", "")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if n != 3 {
		t.Errorf("new generation = %d, want 3", n)
	}
	// Rollback wrote the ops; assert the store now matches gen 1 content.
	e, err := st.Get(ctx, store.Key("/blocks/a"))
	if err != nil || string(e.Value) != "one" {
		t.Errorf("/blocks/a = %v, %v; want one", e, err)
	}
	if _, err := st.Get(ctx, store.Key("/blocks/b")); err == nil {
		t.Error("/blocks/b still exists after rollback")
	}

	// Rolling back to the current generation is a no-op.
	n, err = Rollback(ctx, st, 2, "user", "")
	if err != nil || n != 2 {
		t.Errorf("no-op rollback = %d, %v; want 2, nil", n, err)
	}
	// Target above current is invalid.
	if _, err := Rollback(ctx, st, 99, "user", ""); !errors.Is(err, errors.KindInvalid) {
		t.Errorf("rollback to 99: err = %v, want KindInvalid", err)
	}
}
