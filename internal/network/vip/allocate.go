// Package vip implements VIP allocation and holdership (spec §4.2).
//
// Allocation is a store-driven derived projection: each block that
// requests `expose: vip` owns exactly one record at
// /network/vipPool/<blockRef> naming its address. The allocator hands
// out the lowest free address from the applicable pool and is stable
// for the block's lifetime — the record is deleted only when the block
// is. There is no separate allocation index to drift; the set of
// records IS the allocation state.
//
// Pools:
//   - external: configured range (e.g. 192.168.1.100-192.168.1.120),
//     parsed by ParseExternalPool.
//   - internal: the fixed 10.43.0.0/16 range (addrplan.InternalVIPCIDR),
//     no cluster configuration needed.
package vip

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/network/addrplan"
	"github.com/expanse/expanse/internal/store"
)

// PoolPrefix is the directory holding one allocation record per block.
const PoolPrefix = "/network/vipPool/"

// Scope distinguishes external (LAN-announced) from internal (overlay
// service) VIPs.
type Scope string

const (
	// ScopeInternal VIPs live in the internal service range.
	ScopeInternal Scope = "internal"
	// ScopeExternal VIPs come from the configured external pool and are
	// announced on the physical LAN.
	ScopeExternal Scope = "external"
)

// VIP is the spec §4.2 struct. Address carries the LAN mask for
// external VIPs (e.g. 192.168.1.100/24) so the holder can add it to
// the interface verbatim.
type VIP struct {
	Address   netip.Prefix
	Interface string // physical interface to announce on, "" = auto
	Scope     Scope
	BlockRef  string
	Holder    string // current node ID (status, not persisted here)
}

// allocation is the JSON payload persisted at PoolPrefix/<blockRef>.
type allocation struct {
	Addr  string `json:"addr"` // netip.Prefix String()
	Scope Scope  `json:"scope"`
}

// PoolKey returns the store key for a block's allocation record at
// the given scope. A block may hold one internal AND one external VIP
// (two records); the scope qualifier keeps them independent.
func PoolKey(scope Scope, blockRef string) store.Key {
	return store.Key(PoolPrefix + string(scope) + "/" + blockRef)
}

// ParseExternalPool parses the cluster config's externalVIPPool value:
// a comma-separated list of inclusive address ranges ("a-b", e.g.
// 192.168.1.100-192.168.1.120) or single addresses ("a"). An optional
// "/len" on the item (e.g. "192.168.1.100-192.168.1.120/24" or
// "192.168.1.5/24") sets the prefix mask announced with each address;
// without it, bare IPv4 defaults to /24 (the LAN mask from the spec's
// example). Returned prefixes are single-address (Bits()==8*len).
func ParseExternalPool(cfg string) ([]netip.Prefix, error) {
	cfg = strings.TrimSpace(cfg)
	if cfg == "" {
		return nil, nil
	}
	var out []netip.Prefix
	for _, item := range strings.Split(cfg, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		pfx, err := parsePoolItem(item)
		if err != nil {
			return nil, err
		}
		out = append(out, pfx...)
	}
	return out, nil
}

func parsePoolItem(item string) ([]netip.Prefix, error) {
	// Optional mask suffix: "/24" after the range or address.
	mask := 0
	if i := strings.LastIndex(item, "/"); i >= 0 {
		n, err := fmt.Sscanf(item[i+1:], "%d", &mask)
		if err != nil || n != 1 {
			return nil, errors.New(errors.KindInvalid, "vip", "invalid prefix length in pool item "+item)
		}
		item = item[:i]
	}
	lo, hi, ranged := strings.Cut(item, "-")
	loAddr, err := netip.ParseAddr(strings.TrimSpace(lo))
	if err != nil {
		return nil, errors.Wrap(err, errors.KindInvalid, "vip", "pool start address")
	}
	loAddr = loAddr.Unmap()
	hiAddr := loAddr
	if ranged {
		hiAddr, err = netip.ParseAddr(strings.TrimSpace(hi))
		if err != nil {
			return nil, errors.Wrap(err, errors.KindInvalid, "vip", "pool end address")
		}
		hiAddr = hiAddr.Unmap()
	}
	if hiAddr.Less(loAddr) {
		return nil, errors.New(errors.KindInvalid, "vip", "pool range is reversed: "+item)
	}
	if mask == 0 {
		// Spec example uses the LAN /24; that is the sensible default
		// for bare pool addresses.
		mask = 24
	}
	if mask < 0 || mask > loAddr.BitLen() {
		return nil, errors.New(errors.KindInvalid, "vip", fmt.Sprintf("prefix /%d out of range for %s", mask, item))
	}
	var out []netip.Prefix
	for a := loAddr; ; a = a.Next() {
		out = append(out, netip.PrefixFrom(a, mask))
		if a == hiAddr {
			break
		}
		if !a.Next().IsValid() {
			return nil, errors.New(errors.KindInvalid, "vip", "pool range overflows address space")
		}
	}
	return out, nil
}

// InternalPool returns the internal VIP pool as single-address /32
// prefixes covering 10.43.0.1 through 10.43.255.254 (the /16's usable
// host range — the all-zero and broadcast addresses are excluded).
func InternalPool() []netip.Prefix {
	base := mustPrefix(addrplan.InternalVIPCIDR)
	first := base.Addr().Next() // skip 10.43.0.0
	last := netip.AddrFrom4([4]byte{10, 43, 255, 254})
	var out []netip.Prefix
	for a := first; base.Contains(a); a = a.Next() {
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
		if a == last {
			break
		}
	}
	return out
}

// Allocate assigns the lowest free address from pool to blockRef at
// the given scope. If the block already holds a record it is returned
// unchanged (stability across repeated calls and across restarts).
// The record persists until Release — typically at block delete.
func Allocate(ctx context.Context, st store.Store, blockRef string, scope Scope, pool []netip.Prefix) (netip.Prefix, error) {
	// Block refs are hierarchical ("web/nginx"); slashes are legal
	// in the key. Reject only the empty ref.
	if blockRef == "" {
		return netip.Prefix{}, errors.New(errors.KindInvalid, "vip", "invalid block ref "+blockRef)
	}
	if scope != ScopeInternal && scope != ScopeExternal {
		return netip.Prefix{}, errors.New(errors.KindInvalid, "vip", "unknown scope "+string(scope))
	}
	key := PoolKey(scope, blockRef)
	if ent, err := st.Get(ctx, key); err == nil {
		var a allocation
		if err := json.Unmarshal(ent.Value, &a); err != nil {
			return netip.Prefix{}, errors.Wrap(err, errors.KindInternal, "vip", "corrupt allocation record")
		}
		p, err := netip.ParsePrefix(a.Addr)
		if err != nil {
			return netip.Prefix{}, errors.Wrap(err, errors.KindInternal, "vip", "corrupt allocation record")
		}
		return p, nil
	}
	used, err := usedAddrs(ctx, st)
	if err != nil {
		return netip.Prefix{}, err
	}
	for _, p := range pool {
		addr := p.Addr()
		if _, taken := used[addr]; taken {
			continue
		}
		rec, err := json.Marshal(allocation{Addr: p.String(), Scope: scope})
		if err != nil {
			return netip.Prefix{}, errors.Wrap(err, errors.KindInternal, "vip", "encode allocation")
		}
		// expect==0: create-only. Losing a race with another writer
		// (same address claimed concurrently) surfaces as KindConflict;
		// a single retry pass covers the realistic case.
		if _, err := st.CompareAndSwap(ctx, key, 0, rec); err != nil {
			continue
		}
		return p, nil
	}
	return netip.Prefix{}, errors.New(errors.KindResourceExhausted, "vip",
		fmt.Sprintf("no free address in pool for block %q", blockRef))
}

// Release deletes a block's allocation record. Deleting a block with
// no allocation is a no-op (block delete is idempotent).
func Release(ctx context.Context, st store.Store, blockRef string) error {
	for _, scope := range []Scope{ScopeInternal, ScopeExternal} {
		key := PoolKey(scope, blockRef)
		ent, err := st.Get(ctx, key)
		if err != nil {
			if errors.KindOf(err) == errors.KindNotFound {
				continue // never allocated; idempotent delete
			}
			return err
		}
		if err := st.Delete(ctx, key, ent.Revision); err != nil {
			return err
		}
	}
	return nil
}

func usedAddrs(ctx context.Context, st store.Store) (map[netip.Addr]struct{}, error) {
	ents, err := st.List(ctx, store.Key(PoolPrefix))
	if err != nil {
		return nil, err
	}
	used := make(map[netip.Addr]struct{}, len(ents))
	for _, ent := range ents {
		var a allocation
		if err := json.Unmarshal(ent.Value, &a); err != nil {
			continue // ignore corrupt records; they occupy no address
		}
		if p, err := netip.ParsePrefix(a.Addr); err == nil {
			used[p.Addr()] = struct{}{}
		}
	}
	return used, nil
}

func mustPrefix(s string) netip.Prefix {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		panic(err)
	}
	return p
}
