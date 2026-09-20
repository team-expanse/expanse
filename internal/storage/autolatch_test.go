package storage

import (
	"context"
	"testing"
)

func TestAutoLatchSurvivesAndClears(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if AutoLatched(ctx, st, "vol-a") {
		t.Fatal("a volume nobody latched must not read as auto-latched")
	}
	if err := SetAutoLatch(ctx, st, "vol-a"); err != nil {
		t.Fatal(err)
	}
	if !AutoLatched(ctx, st, "vol-a") || AutoLatched(ctx, st, "vol-b") {
		t.Fatal("the marker is per volume and durable in the store")
	}
	ClearAutoLatch(ctx, st, "vol-a")
	ClearAutoLatch(ctx, st, "vol-a") // clearing twice is fine
	if AutoLatched(ctx, st, "vol-a") {
		t.Fatal("cleared marker still set")
	}
}

func TestAutoLatchMarkerIsNotAVolume(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if err := SetAutoLatch(ctx, st, "vol-ghost"); err != nil {
		t.Fatal(err)
	}
	ids, err := ListVolumeIDs(ctx, st)
	if err != nil || len(ids) != 0 {
		t.Fatalf("ids = %v, err = %v: a marker alone must not create a volume", ids, err)
	}
}
