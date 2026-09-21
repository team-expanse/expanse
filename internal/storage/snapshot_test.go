package storage

import (
	"context"
	"reflect"
	"testing"
	"time"

	experrors "github.com/expanse/expanse/internal/errors"
)

func TestSnapshotRecordsRoundTripInNameOrder(t *testing.T) {
	st, ctx := newStore(t), context.Background()
	at := time.Unix(1700000000, 0).UTC()
	for _, name := range []string{"b", "a"} {
		if err := PutSnapshot(ctx, st, "vol-1", SnapshotRecord{Name: name, Node: "n1", CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ListSnapshotRecords(ctx, st, "vol-1")
	if err != nil {
		t.Fatal(err)
	}
	want := []SnapshotRecord{{Name: "a", Node: "n1", CreatedAt: at}, {Name: "b", Node: "n1", CreatedAt: at}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestSnapshotRecordsAreScopedToTheirVolume(t *testing.T) {
	st, ctx := newStore(t), context.Background()
	_ = PutSnapshot(ctx, st, "vol-1", SnapshotRecord{Name: "a", Node: "n1"})
	_ = PutSnapshot(ctx, st, "vol-10", SnapshotRecord{Name: "z", Node: "n1"})
	got, _ := ListSnapshotRecords(ctx, st, "vol-1")
	if len(got) != 1 || got[0].Name != "a" {
		t.Errorf("vol-1 sees %+v; a prefix match leaked vol-10's snapshot", got)
	}
}

func TestSnapshotRecordsDoNotAppearAsVolumes(t *testing.T) {
	st, ctx := newStore(t), context.Background()
	_ = PutSnapshot(ctx, st, "vol-1", SnapshotRecord{Name: "a", Node: "n1"})
	ids, err := ListVolumeIDs(ctx, st)
	if err != nil || len(ids) != 0 {
		t.Errorf("ListVolumeIDs = %v, %v; want none", ids, err)
	}
}

func TestDeleteSnapshotRecordsDropsOnlyThatVolumesRecords(t *testing.T) {
	st, ctx := newStore(t), context.Background()
	_ = PutSnapshot(ctx, st, "vol-1", SnapshotRecord{Name: "a", Node: "n1"})
	_ = PutSnapshot(ctx, st, "vol-10", SnapshotRecord{Name: "z", Node: "n1"})
	if err := DeleteSnapshotRecords(ctx, st, "vol-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := ListSnapshotRecords(ctx, st, "vol-1"); len(got) != 0 {
		t.Errorf("vol-1 still has %+v", got)
	}
	if got, _ := ListSnapshotRecords(ctx, st, "vol-10"); len(got) != 1 {
		t.Errorf("vol-10 lost its snapshot: %+v", got)
	}
}

func TestSnapshotNameValidation(t *testing.T) {
	for _, ok := range []string{"a", "snap-1", "before-upgrade", "x0"} {
		if err := ValidSnapshotName(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-a", "A", "a b", "a/b", "a_b", "snapshot.1", string(make([]byte, 64))} {
		if err := ValidSnapshotName(bad); experrors.KindOf(err) != experrors.KindInvalid {
			t.Errorf("%q: kind %v, want invalid", bad, experrors.KindOf(err))
		}
	}
}
