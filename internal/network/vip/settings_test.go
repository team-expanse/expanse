package vip

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
)

func TestSettingsRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestStore(t)
	got, err := LoadSettings(ctx, st)
	if err != nil || got.ExternalPool != "" {
		t.Fatalf("unset settings = %+v, %v; want zero, nil", got, err)
	}
	if err := SaveSettings(ctx, st, Settings{ExternalPool: "192.168.1.100-192.168.1.110"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadSettings(ctx, st); got.ExternalPool != "192.168.1.100-192.168.1.110" {
		t.Errorf("loaded %+v", got)
	}
	if err := SaveSettings(ctx, st, Settings{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, SettingsKey); experrors.KindOf(err) != experrors.KindNotFound {
		t.Errorf("clearing the pool should delete %s, got %v", SettingsKey, err)
	}
	if err := SaveSettings(ctx, st, Settings{}); err != nil {
		t.Errorf("clearing twice: %v", err)
	}
}

func TestEffectivePool(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ setting, fallback, want string }{
		{"", "", ""},
		{"", "10.0.0.5", "10.0.0.5"},
		{"192.168.1.9", "10.0.0.5", "192.168.1.9"},
	} {
		if got := EffectivePool(Settings{ExternalPool: tt.setting}, tt.fallback); got != tt.want {
			t.Errorf("EffectivePool(%q, %q) = %q, want %q", tt.setting, tt.fallback, got, tt.want)
		}
	}
}

func TestCheckPool(t *testing.T) {
	t.Parallel()
	nodes := []netip.Addr{netip.MustParseAddr("192.168.1.10"), netip.MustParseAddr("192.168.1.11")}
	for _, tt := range []struct {
		pool, wantErr string
	}{
		{"192.168.1.100-192.168.1.120", ""},
		{"192.168.1.100-192.168.1.120, 192.168.1.200", ""},
		{"nonsense", "pool start address"},
		{"10.0.0.100-10.0.0.120", "no node is on 10.0.0.0/24"},
		{"192.168.1.5-192.168.1.12", "192.168.1.10 is a node's address"},
		{"192.168.1.0-192.168.1.3", "192.168.1.0 is the network address of 192.168.1.0/24"},
		{"192.168.1.250-192.168.1.255", "192.168.1.255 is the broadcast address of 192.168.1.0/24"},
		{"192.168.0.1-192.168.15.254/16", "more than 1024 addresses"},
		{"10.43.0.10", "is inside the cluster's own range 10.43.0.0/16"},
	} {
		err := CheckPool(tt.pool, nodes)
		switch {
		case tt.wantErr == "" && err != nil:
			t.Errorf("CheckPool(%q) = %v, want ok", tt.pool, err)
		case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
			t.Errorf("CheckPool(%q) = %v, want error containing %q", tt.pool, err, tt.wantErr)
		}
	}
	if err := CheckPool("10.0.0.100", nil); err != nil {
		t.Errorf("with no known node addresses only the syntax is checked, got %v", err)
	}
}

// A record outside the current pool moves into it, so a changed range takes effect.
func TestAllocateMovesARecordOutsideThePool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestStore(t)
	first, err := Allocate(ctx, st, "web/nginx", ScopeExternal, mustPool("192.168.1.100-192.168.1.101"))
	if err != nil {
		t.Fatal(err)
	}
	moved, err := Allocate(ctx, st, "web/nginx", ScopeExternal, mustPool("192.168.1.200-192.168.1.201"))
	if err != nil {
		t.Fatal(err)
	}
	if moved == first || moved.String() != "192.168.1.200/24" {
		t.Errorf("after the range changed the block kept %s (now %s), want 192.168.1.200/24", first, moved)
	}
	if again, _ := Allocate(ctx, st, "web/nginx", ScopeExternal, mustPool("192.168.1.200-192.168.1.201")); again != moved {
		t.Errorf("not stable after moving: %s then %s", moved, again)
	}
}

// Assign uses the LAN pool when one is set, else the internal one, and drops the other scope's record.
func TestAssignSwitchesScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestStore(t)
	in, scope, err := Assign(ctx, st, "web/nginx", nil)
	if err != nil || scope != ScopeInternal || !strings.HasPrefix(in.String(), "10.43.") {
		t.Fatalf("no pool: %s %s %v, want an internal address", in, scope, err)
	}
	ext, scope, err := Assign(ctx, st, "web/nginx", mustPool("192.168.1.100"))
	if err != nil || scope != ScopeExternal || ext.String() != "192.168.1.100/24" {
		t.Fatalf("with a pool: %s %s %v", ext, scope, err)
	}
	if _, err := st.Get(ctx, PoolKey(ScopeInternal, "web/nginx")); experrors.KindOf(err) != experrors.KindNotFound {
		t.Errorf("internal record kept after moving to the LAN: %v", err)
	}
	if p, s, ok := Lookup(ctx, st, "web/nginx"); !ok || p != ext || s != ScopeExternal {
		t.Errorf("Lookup = %s %s %v, want %s external", p, s, ok, ext)
	}
	if _, _, err := Assign(ctx, st, "web/nginx", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, PoolKey(ScopeExternal, "web/nginx")); experrors.KindOf(err) != experrors.KindNotFound {
		t.Errorf("external record kept after the pool was cleared: %v", err)
	}
	if _, _, ok := Lookup(ctx, st, "db/none"); ok {
		t.Error("Lookup found an address for a block without one")
	}
}
