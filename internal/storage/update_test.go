package storage

import (
	"context"
	"sync/atomic"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// flakyCAS fails the first n compare-and-swaps the way a lost race does.
type flakyCAS struct {
	store.Store
	fail atomic.Int32
}

func (f *flakyCAS) CompareAndSwap(ctx context.Context, k store.Key, expect store.Revision, v []byte) (store.Revision, error) {
	if f.fail.Add(-1) >= 0 {
		return 0, experrors.New(experrors.KindConflict, "test", "lost the race")
	}
	return f.Store.CompareAndSwap(ctx, k, expect, v)
}

func TestUpdateStatusRetriesALostRaceAndChangesOnlyItsField(t *testing.T) {
	ctx := context.Background()
	f := &flakyCAS{Store: newStore(t)}
	if err := SaveStatus(ctx, f, "vol-u", Status{State: StateHealthy, Placement: []Replica{{NodeID: "n1", Role: RoleStale}, {NodeID: "n2", Role: RoleSecondary}}}); err != nil {
		t.Fatal(err)
	}
	f.fail.Store(3)
	err := UpdateStatus(ctx, f, "vol-u", func(s *Status) bool {
		s.Placement[0].Role = RoleSecondary
		return true
	})
	if err != nil {
		t.Fatalf("UpdateStatus gave up after 3 lost races: %v", err)
	}
	got, _, _ := LoadStatus(ctx, f, "vol-u")
	if got.Placement[0].Role != RoleSecondary || got.Placement[1].Role != RoleSecondary || got.State != StateHealthy {
		t.Fatalf("status = %+v", got)
	}
}

func TestUpdateStatusReportsPersistentConflictAndSkipsNoChange(t *testing.T) {
	ctx := context.Background()
	f := &flakyCAS{Store: newStore(t)}
	_ = SaveStatus(ctx, f, "vol-u", Status{State: StateHealthy})
	f.fail.Store(1000)
	if err := UpdateStatus(ctx, f, "vol-u", func(s *Status) bool { s.Primary = "n1"; return true }); err == nil {
		t.Fatal("a persistent conflict must surface, not vanish")
	}
	f.fail.Store(0)
	rev := func() store.Revision { _, r, _ := LoadStatus(ctx, f, "vol-u"); return r }
	before := rev()
	if err := UpdateStatus(ctx, f, "vol-u", func(*Status) bool { return false }); err != nil || rev() != before {
		t.Fatalf("a no-op update must not write (err=%v)", err)
	}
}
