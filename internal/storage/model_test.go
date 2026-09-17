package storage

import (
	"context"
	"testing"
	"time"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

func newStore(t *testing.T) store.Store {
	t.Helper()
	st, err := boltstore.New(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testVolume() Volume {
	return Volume{
		ID:          "vol-1a2b3c4d5e6f7a8b",
		Name:        "pgdata",
		Namespace:   "default",
		SizeBytes:   10 << 30,
		Class:       "default",
		Replication: 3,
		Placement: []Replica{
			{NodeID: "n1", Role: RolePrimary, ZvolPath: "rpool/volumes/vol-1a2b3c4d5e6f7a8b", Sequence: 42, LastSeen: time.Unix(1700000000, 0).UTC(), Healthy: true},
			{NodeID: "n2", Role: RoleSecondary, ZvolPath: "rpool/volumes/vol-1a2b3c4d5e6f7a8b", Sequence: 42, LastSeen: time.Unix(1700000000, 0).UTC(), Healthy: true},
			{NodeID: "n3", Role: RoleSecondary, ZvolPath: "rpool/volumes/vol-1a2b3c4d5e6f7a8b", Sequence: 40, LastSeen: time.Unix(1699999999, 0).UTC(), Healthy: true},
		},
		Generation: 7,
		State:      StateHealthy,
		Primary:    "n1",
		Sequence:   42,
	}
}

func TestSaveLoadSpecRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	v := testVolume()

	if err := SaveSpec(ctx, st, v.Spec()); err != nil {
		t.Fatalf("SaveSpec: %v", err)
	}
	got, err := LoadSpec(ctx, st, v.ID)
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	want := v.Spec()
	if got != want {
		t.Errorf("spec mismatch: got %+v, want %+v", got, want)
	}
}

func TestLoadSpecNotFound(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	_, err := LoadSpec(ctx, st, "vol-nonexistent")
	if experrors.KindOf(err) != experrors.KindNotFound {
		t.Errorf("KindOf = %v, want not_found", experrors.KindOf(err))
	}
}

func TestSaveLoadStatusRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	v := testVolume()

	if err := SaveStatus(ctx, st, v.ID, v.Status()); err != nil {
		t.Fatalf("SaveStatus: %v", err)
	}
	got, _, err := LoadStatus(ctx, st, v.ID)
	if err != nil {
		t.Fatalf("LoadStatus: %v", err)
	}
	want := v.Status()
	if got.Generation != want.Generation || got.State != want.State ||
		got.Primary != want.Primary || got.Sequence != want.Sequence {
		t.Errorf("scalar fields mismatch: got %+v, want %+v", got, want)
	}
	if len(got.Placement) != len(want.Placement) {
		t.Fatalf("placement length: got %d, want %d", len(got.Placement), len(want.Placement))
	}
	for i := range want.Placement {
		if got.Placement[i] != want.Placement[i] {
			t.Errorf("placement[%d]: got %+v, want %+v", i, got.Placement[i], want.Placement[i])
		}
	}
}

func TestLoadStatusNotFound(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	_, _, err := LoadStatus(ctx, st, "vol-nonexistent")
	if experrors.KindOf(err) != experrors.KindNotFound {
		t.Errorf("KindOf = %v, want not_found", experrors.KindOf(err))
	}
}

func TestCompareAndSwapStatus(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	v := testVolume()

	if err := SaveStatus(ctx, st, v.ID, v.Status()); err != nil {
		t.Fatalf("SaveStatus: %v", err)
	}
	status, rev, err := LoadStatus(ctx, st, v.ID)
	if err != nil {
		t.Fatalf("LoadStatus: %v", err)
	}

	// Stale expect must fail with KindConflict.
	status.State = StateDegraded
	err = CompareAndSwapStatus(ctx, st, v.ID, rev-1, status)
	if experrors.KindOf(err) != experrors.KindConflict {
		t.Errorf("stale CAS KindOf = %v, want conflict", experrors.KindOf(err))
	}

	// Current expect succeeds.
	if err := CompareAndSwapStatus(ctx, st, v.ID, rev, status); err != nil {
		t.Fatalf("CAS: %v", err)
	}
	got, rev2, err := LoadStatus(ctx, st, v.ID)
	if err != nil {
		t.Fatalf("LoadStatus: %v", err)
	}
	if got.State != StateDegraded {
		t.Errorf("State = %v, want Degraded", got.State)
	}
	if rev2 <= rev {
		t.Errorf("revision did not advance: %d -> %d", rev, rev2)
	}
}

func TestListVolumeIDs(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	if err := SaveSpec(ctx, st, testVolume().Spec()); err != nil {
		t.Fatalf("SaveSpec: %v", err)
	}
	other := testVolume()
	other.ID = "vol-aaaaaaaaaaaaaaaa"
	if err := SaveSpec(ctx, st, other.Spec()); err != nil {
		t.Fatalf("SaveSpec: %v", err)
	}
	// Status keys must not create phantom IDs.
	if err := SaveStatus(ctx, st, testVolume().ID, testVolume().Status()); err != nil {
		t.Fatalf("SaveStatus: %v", err)
	}

	ids, err := ListVolumeIDs(ctx, st)
	if err != nil {
		t.Fatalf("ListVolumeIDs: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("got %v, want 2 ids", ids)
	}
	found := map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if !found["vol-1a2b3c4d5e6f7a8b"] || !found["vol-aaaaaaaaaaaaaaaa"] {
		t.Errorf("missing ids: %v", ids)
	}
}

func TestStateAndRoleProtoRoundTrip(t *testing.T) {
	for s := VolumeState(StateCreating); ; s = next(s) {
		if got := stateFromProto(s.proto()); got != s {
			t.Errorf("state %q round-tripped as %q", s, got)
		}
		if s == StateDeleting {
			break
		}
	}
	for _, r := range []Role{RolePrimary, RoleSecondary, RoleResyncing, RoleStale} {
		if got := roleFromProto(r.proto()); got != r {
			t.Errorf("role %q round-tripped as %q", r, got)
		}
	}
	if got := stateFromProto(0); got != "" {
		t.Errorf("unspecified state decoded to %q, want empty", got)
	}
	if got := roleFromProto(0); got != "" {
		t.Errorf("unspecified role decoded to %q, want empty", got)
	}
}

func next(s VolumeState) VolumeState {
	switch s {
	case StateCreating:
		return StateHealthy
	case StateHealthy:
		return StateDegraded
	case StateDegraded:
		return StateReadOnly
	case StateReadOnly:
		return StateResyncing
	case StateResyncing:
		return StateFailed
	case StateFailed:
		return StateDeleting
	}
	return StateDeleting
}
