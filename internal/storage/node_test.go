package storage

import (
	"context"
	"testing"
)

func TestEnableRequestRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if _, ok, err := LoadEnableRequest(ctx, st, "n1"); err != nil || ok {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}
	want := EnableRequest{VG: "expanse", Pool: "pool", Percent: 80}
	if err := SaveEnableRequest(ctx, st, "n1", want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadEnableRequest(ctx, st, "n1")
	if err != nil || !ok || got != want {
		t.Fatalf("got %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}
}

func TestEnableRequestDefaults(t *testing.T) {
	r := EnableRequest{}.WithDefaults()
	if r.VG != DefaultVG || r.Pool != DefaultPool || r.Percent != DefaultPercent {
		t.Fatalf("defaults = %+v", r)
	}
}

func TestEnableRequestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  EnableRequest
		ok   bool
	}{
		{"defaults", EnableRequest{}.WithDefaults(), true},
		{"percent too low", EnableRequest{VG: "expanse", Pool: "pool", Percent: 0}, false},
		{"percent too high", EnableRequest{VG: "expanse", Pool: "pool", Percent: 99}, false},
		{"bad vg", EnableRequest{VG: "-x", Pool: "pool", Percent: 50}, false},
		{"bad pool", EnableRequest{VG: "expanse", Pool: "a/b", Percent: 50}, false},
	} {
		if err := tc.req.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
}

func TestNodeStorageStateRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	want := NodeStorage{State: NodeStorageReady, Source: SourceUI, VG: "expanse", Pool: "pool", VGSize: 100, VGFree: 5, PoolSize: 95}
	if err := SaveNodeStorage(ctx, st, "n1", want); err != nil {
		t.Fatal(err)
	}
	all, err := ListNodeStorage(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if got := all["n1"]; got != want || len(all) != 1 {
		t.Fatalf("got %+v, want %+v", all, want)
	}
}
