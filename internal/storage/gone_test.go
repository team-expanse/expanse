package storage

import (
	"context"
	"reflect"
	"testing"
)

func TestGoneMarksRoundTripPerNode(t *testing.T) {
	st, ctx := newStore(t), context.Background()
	for _, vol := range []string{"vol-b", "vol-a"} {
		if err := PutGone(ctx, st, "n1", vol); err != nil {
			t.Fatal(err)
		}
	}
	if err := PutGone(ctx, st, "n2", "vol-c"); err != nil {
		t.Fatal(err)
	}
	got, err := ListGone(ctx, st, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"vol-a", "vol-b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("n1 sees %v, want %v", got, want)
	}
}

func TestGoneMarksAreScopedToTheirNode(t *testing.T) {
	st, ctx := newStore(t), context.Background()
	_ = PutGone(ctx, st, "n1", "vol-a")
	_ = PutGone(ctx, st, "n10", "vol-z")
	got, _ := ListGone(ctx, st, "n1")
	if !reflect.DeepEqual(got, []string{"vol-a"}) {
		t.Errorf("n1 sees %v; a prefix match leaked n10's mark", got)
	}
}

func TestDeleteGoneDropsOneMark(t *testing.T) {
	st, ctx := newStore(t), context.Background()
	_ = PutGone(ctx, st, "n1", "vol-a")
	_ = PutGone(ctx, st, "n1", "vol-b")
	if err := DeleteGone(ctx, st, "n1", "vol-a"); err != nil {
		t.Fatal(err)
	}
	got, _ := ListGone(ctx, st, "n1")
	if !reflect.DeepEqual(got, []string{"vol-b"}) {
		t.Errorf("got %v, want only vol-b", got)
	}
}

func TestGoneMarksDoNotAppearAsVolumes(t *testing.T) {
	st, ctx := newStore(t), context.Background()
	_ = PutGone(ctx, st, "n1", "vol-a")
	if ids, err := ListVolumeIDs(ctx, st); err != nil || len(ids) != 0 {
		t.Errorf("ListVolumeIDs = %v, %v; want none", ids, err)
	}
}
