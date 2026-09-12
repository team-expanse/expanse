package raftstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/cluster/generation"
	"github.com/expanse/expanse/internal/store"
)

// TestGenerationsPerMutation (§4.7): every accepted desired-state
// mutation creates a new generation atomically; status keys never do.
func TestGenerationsPerMutation(t *testing.T) {
	tc := NewTestCluster(t, 3)
	defer tc.Close()
	ctx := context.Background()
	st := tc.Leader()

	// Status-key mutation: no generation.
	if _, err := st.Put(ctx, store.Key("/nodes/n1/status"), []byte("ready")); err != nil {
		t.Fatalf("Put status: %v", err)
	}
	if n, _ := generation.Current(ctx, st); n != 0 {
		t.Fatalf("status mutation created generation %d, want none", n)
	}

	// Three desired-state changes → generations 1, 2, 3.
	if _, err := st.Put(ctx, store.Key("/blocks/a"), []byte("v1")); err != nil {
		t.Fatalf("Put a: %v", err)
	}
	if _, err := st.Put(ctx, store.Key("/blocks/a"), []byte("v2")); err != nil {
		t.Fatalf("Put a: %v", err)
	}
	if _, err := st.Txn(ctx, []store.Op{
		{Kind: store.OpPut, Key: store.Key("/volumes/b"), Value: []byte("v3")},
	}); err != nil {
		t.Fatalf("Txn: %v", err)
	}

	cur, err := generation.Current(ctx, st)
	if err != nil || cur != 3 {
		t.Fatalf("Current = %d, %v; want 3", cur, err)
	}

	// Snapshots reflect the state at each revision.
	c1, err := generation.Content(ctx, st, 1)
	if err != nil {
		t.Fatalf("Content(1): %v", err)
	}
	if string(c1[store.Key("/blocks/a")]) != "v1" || len(c1) != 1 {
		t.Errorf("gen1 = %v", c1)
	}
	c3, _ := generation.Content(ctx, st, 3)
	if string(c3[store.Key("/blocks/a")]) != "v2" || string(c3[store.Key("/volumes/b")]) != "v3" || len(c3) != 2 {
		t.Errorf("gen3 = %v", c3)
	}

	// History is linear: each generation's parent is the previous number.
	gens, err := generation.List(ctx, st)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(gens) != 3 {
		t.Fatalf("List = %d gens, want 3", len(gens))
	}
	for i, g := range gens {
		if g.Number != uint64(i+1) || g.Parent != uint64(i) {
			t.Errorf("gens[%d] = {num:%d parent:%d}", i, g.Number, g.Parent)
		}
		if g.Hash != generation.Hash(mustContent(t, ctx, st, g.Number)) {
			t.Errorf("gen %d hash mismatch with stored data", g.Number)
		}
	}

	// Replicated: followers see the same generation history.
	fol := tc.Follower()
	fcur, err := generation.Current(ctx, fol)
	if err != nil || fcur != 3 {
		t.Fatalf("follower Current = %d, %v; want 3", fcur, err)
	}
}

// TestRollbackAppendOnly (§4.7 / G3.11): apply A, B, C; roll back to the
// generation holding A; the new generation's hash equals the old one's
// and the full history is preserved. Roll forward = roll back again.
func TestRollbackAppendOnly(t *testing.T) {
	tc := NewTestCluster(t, 3)
	defer tc.Close()
	ctx := context.Background()
	st := tc.Leader()

	// A: two keys (gen 1).
	if _, err := st.Txn(ctx, []store.Op{
		{Kind: store.OpPut, Key: store.Key("/blocks/web/replicas"), Value: []byte("2")},
		{Kind: store.OpPut, Key: store.Key("/blocks/web/image"), Value: []byte("web:1")},
	}); err != nil {
		t.Fatalf("apply A: %v", err)
	}
	// B (gen 2): change image, add volume.
	if _, err := st.Txn(ctx, []store.Op{
		{Kind: store.OpPut, Key: store.Key("/blocks/web/image"), Value: []byte("web:2")},
		{Kind: store.OpPut, Key: store.Key("/volumes/data"), Value: []byte("10G")},
	}); err != nil {
		t.Fatalf("apply B: %v", err)
	}
	// C (gen 3): delete a key, change a value.
	if _, err := st.Txn(ctx, []store.Op{
		{Kind: store.OpDelete, Key: store.Key("/blocks/web/replicas")},
		{Kind: store.OpPut, Key: store.Key("/volumes/data"), Value: []byte("20G")},
	}); err != nil {
		t.Fatalf("apply C: %v", err)
	}

	hashG1 := mustContentHash(t, ctx, st, 1)
	hashG3 := mustContentHash(t, ctx, st, 3)
	if hashG1 == hashG3 {
		t.Fatal("test invariant broken: gen1 and gen3 identical")
	}

	// Rollback to generation 1 → becomes generation 4 with gen 1's content.
	n, err := generation.Rollback(ctx, st, 1, "user", "undo B and C")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if n != 4 {
		t.Fatalf("new generation = %d, want 4", n)
	}

	// The live desired state now equals gen 1's content.
	e, err := st.Get(ctx, store.Key("/blocks/web/replicas"))
	if err != nil || string(e.Value) != "2" {
		t.Errorf("replicas = %v, %v; want 2", e, err)
	}
	if _, err := st.Get(ctx, store.Key("/volumes/data")); err == nil {
		t.Error("/volumes/data survived rollback to gen 1")
	}
	if got := mustContentHash(t, ctx, st, 4); got != hashG1 {
		t.Errorf("hash(gen4) = %s, want hash(gen1) = %s", got, hashG1)
	}

	// History preserved: gens 1..4 all listed, gen 2/3 intact.
	gens, err := generation.List(ctx, st)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(gens) != 4 {
		t.Fatalf("List = %d gens, want 4 (append-only history)", len(gens))
	}
	if mustContentHash(t, ctx, st, 2) == mustContentHash(t, ctx, st, 1) {
		t.Error("gen 2 was rewritten")
	}

	// Roll forward: rolling back to gen 3 restores it as gen 5.
	n, err = generation.Rollback(ctx, st, 3, "user", "roll forward again")
	if err != nil {
		t.Fatalf("Rollback forward: %v", err)
	}
	if n != 5 {
		t.Fatalf("roll-forward generation = %d, want 5", n)
	}
	if got := mustContentHash(t, ctx, st, 5); got != hashG3 {
		t.Errorf("hash(gen5) = %s, want hash(gen3) = %s", got, hashG3)
	}
}

// TestGenerationRetentionWindow: generations newer than 30 days are all
// retained even past KeepLast (spec: keep last 50 PLUS everything from
// the last 30 days); genuinely old ones beyond the newest 50 are pruned.
func TestGenerationRetentionWindow(t *testing.T) {
	tc := NewTestCluster(t, 1)
	defer tc.Close()
	ctx := context.Background()
	st := tc.Nodes[0]

	for i := 0; i < 55; i++ {
		if _, err := st.Put(ctx, store.Key("/blocks/key"), []byte{byte(i)}); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	gens, err := generation.List(ctx, st)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(gens) != 55 {
		t.Fatalf("fresh generations retained = %d, want 55 (all within 30-day window)", len(gens))
	}
}

// TestFSMGenerationClockJump: a generation's CreatedAt comes from the
// leader's command timestamp, so a wildly wrong wall clock only shifts
// metadata — it cannot corrupt numbering or snapshots.
func TestFSMGenerationClockJump(t *testing.T) {
	tc := NewTestCluster(t, 1)
	defer tc.Close()
	ctx := context.Background()
	st := tc.Nodes[0]

	if _, err := st.Put(ctx, store.Key("/blocks/a"), []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	g, err := generation.Get(ctx, st, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if age := time.Since(g.CreatedAt); age < -time.Hour && age > 24*time.Hour {
		t.Errorf("CreatedAt absurdly far from wall clock: %v", g.CreatedAt)
	}
}

func mustContent(t *testing.T, ctx context.Context, st store.Store, n uint64) map[store.Key][]byte {
	t.Helper()
	m, err := generation.Content(ctx, st, n)
	if err != nil {
		t.Fatalf("Content(%d): %v", n, err)
	}
	return m
}

func mustContentHash(t *testing.T, ctx context.Context, st store.Store, n uint64) string {
	t.Helper()
	return generation.Hash(mustContent(t, ctx, st, n))
}
