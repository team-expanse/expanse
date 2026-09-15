package vip

import (
	"context"
	"net/netip"
	"path/filepath"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "bolt.db"))
	if err != nil {
		t.Fatalf("boltstore.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// extPool is a small external pool for exhaustion testing:
// 192.168.1.100 - 192.168.1.102, three addresses.
var extPool = mustPool("192.168.1.100-192.168.1.102")

func mustPool(cfg string) []netip.Prefix {
	p, err := ParseExternalPool(cfg)
	if err != nil {
		panic(err)
	}
	return p
}

func TestParseExternalPool(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cfg     string
		want    []string
		wantErr bool
	}{
		{name: "empty", cfg: "", want: nil},
		{
			name: "spec example range", cfg: "192.168.1.100-192.168.1.120",
			want: rangeStrings("192.168.1.100", "192.168.1.120", 24),
		},
		{name: "single address", cfg: "192.168.1.5", want: []string{"192.168.1.5/24"}},
		{
			name: "range with mask", cfg: "10.0.0.10-10.0.0.12/24",
			want: rangeStrings("10.0.0.10", "10.0.0.12", 24),
		},
		{
			name: "comma separated", cfg: "192.168.1.5, 10.0.0.1-10.0.0.3/24",
			want: []string{"192.168.1.5/24", "10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24"},
		},
		{name: "single-address mask /32", cfg: "192.168.1.5/32", want: []string{"192.168.1.5/32"}},
		{name: "reversed range", cfg: "192.168.1.9-192.168.1.2", wantErr: true},
		{name: "garbage", cfg: "not-an-ip", wantErr: true},
		{name: "bad mask", cfg: "192.168.1.5/99", wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseExternalPool(tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseExternalPool(%q) = %v, want error", tt.cfg, got)
				}
				if experrors.KindOf(err) != experrors.KindInvalid {
					t.Fatalf("ParseExternalPool(%q) err kind = %v, want invalid", tt.cfg, experrors.KindOf(err))
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseExternalPool(%q): %v", tt.cfg, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("ParseExternalPool(%q) len = %d (%v), want %d (%v)", tt.cfg, len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i].String() != tt.want[i] {
					t.Errorf("item %d = %s, want %s", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func rangeStrings(lo, hi string, mask int) []string {
	start, _ := netip.ParseAddr(lo)
	end, _ := netip.ParseAddr(hi)
	var out []string
	for a := start; ; a = a.Next() {
		out = append(out, netip.PrefixFrom(a, mask).String())
		if a == end {
			break
		}
	}
	return out
}

func TestAllocateLowestFreeAndStability(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestStore(t)

	first, err := Allocate(ctx, st, "web/nginx", ScopeExternal, extPool)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got, want := first.String(), "192.168.1.100/24"; got != want {
		t.Fatalf("first allocation = %s, want %s (lowest free)", got, want)
	}

	second, err := Allocate(ctx, st, "db/redis", ScopeExternal, extPool)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got, want := second.String(), "192.168.1.101/24"; got != want {
		t.Fatalf("second allocation = %s, want %s", got, want)
	}

	// Stability: repeated calls for the same block return the same
	// address without consuming more of the pool.
	for i := 0; i < 5; i++ {
		got, err := Allocate(ctx, st, "web/nginx", ScopeExternal, extPool)
		if err != nil {
			t.Fatalf("Allocate repeat %d: %v", i, err)
		}
		if got != first {
			t.Fatalf("repeat %d = %s, want stable %s", i, got, first)
		}
	}

	// Release the lowest, then allocate a new block: it gets the freed
	// lowest address, not the one after the previous high-water mark.
	if err := Release(ctx, st, "web/nginx"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	third, err := Allocate(ctx, st, "web/caddy", ScopeExternal, extPool)
	if err != nil {
		t.Fatalf("Allocate after release: %v", err)
	}
	if got, want := third.String(), "192.168.1.100/24"; got != want {
		t.Fatalf("allocation after release = %s, want %s", got, want)
	}

	// Releasing a block with no allocation is a no-op.
	if err := Release(ctx, st, "no/such-block"); err != nil {
		t.Fatalf("Release of unknown block: %v", err)
	}
}

func TestAllocateExhaustion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestStore(t)

	for i := range 3 {
		if _, err := Allocate(ctx, st, "block"+string(rune('a'+i)), ScopeExternal, extPool); err != nil {
			t.Fatalf("Allocate %d: %v", i, err)
		}
	}
	_, err := Allocate(ctx, st, "overflow", ScopeExternal, extPool)
	if err == nil {
		t.Fatal("Allocate on exhausted pool: want error")
	}
	if experrors.KindOf(err) != experrors.KindResourceExhausted {
		t.Fatalf("exhaustion err kind = %v (%v), want resource_exhausted", experrors.KindOf(err), err)
	}

	// Freeing capacity makes it succeed again.
	if err := Release(ctx, st, "blocka"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got, err := Allocate(ctx, st, "overflow", ScopeExternal, extPool); err != nil {
		t.Fatalf("Allocate after freeing: %v", err)
	} else if got.String() != "192.168.1.100/24" {
		t.Fatalf("Allocate after freeing = %s, want 192.168.1.100/24", got)
	}
}

func TestAllocateInternalScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestStore(t)

	pool := InternalPool()
	first, err := Allocate(ctx, st, "svc/a", ScopeInternal, pool)
	if err != nil {
		t.Fatalf("Allocate internal: %v", err)
	}
	if got, want := first.String(), "10.43.0.1/32"; got != want {
		t.Fatalf("first internal = %s, want %s", got, want)
	}
	second, err := Allocate(ctx, st, "svc/b", ScopeInternal, pool)
	if err != nil {
		t.Fatalf("Allocate internal 2: %v", err)
	}
	if got, want := second.String(), "10.43.0.2/32"; got != want {
		t.Fatalf("second internal = %s, want %s", got, want)
	}

	// Internal and external allocations must not collide: the same
	// block can hold one of each.
	ext, err := Allocate(ctx, st, "svc/a", ScopeExternal, extPool)
	if err != nil {
		t.Fatalf("Allocate external for same block: %v", err)
	}
	if ext.Addr() == first.Addr() {
		t.Fatalf("external allocation %s collides with internal %s", ext, first)
	}
}

func TestAllocateValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestStore(t)

	if _, err := Allocate(ctx, st, "", ScopeExternal, extPool); experrors.KindOf(err) != experrors.KindInvalid {
		t.Fatalf("empty block ref: kind = %v, want invalid", experrors.KindOf(err))
	}
	if _, err := Allocate(ctx, st, "b", Scope("wan"), extPool); experrors.KindOf(err) != experrors.KindInvalid {
		t.Fatalf("bad scope: kind = %v, want invalid", experrors.KindOf(err))
	}
	if _, err := Allocate(ctx, st, "b", ScopeExternal, nil); experrors.KindOf(err) != experrors.KindResourceExhausted {
		t.Fatalf("empty pool: kind = %v, want resource_exhausted", experrors.KindOf(err))
	}
}
