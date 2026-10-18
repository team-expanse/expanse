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

// SettingsKey holds the cluster's network settings, edited from the web UI.
const SettingsKey = store.Key("/settings/network")

// MaxPoolSize bounds a pool set from the UI; a LAN range is rarely more than a few dozen.
const MaxPoolSize = 1024

// Settings is the JSON stored at SettingsKey.
type Settings struct {
	// ExternalPool is the LAN VIP pool in ParseExternalPool's syntax; empty means unset.
	ExternalPool string `json:"externalVIPPool,omitempty"`
}

// LoadSettings reads the network settings; unset settings are the zero value.
func LoadSettings(ctx context.Context, st store.Store) (Settings, error) {
	var s Settings
	ent, err := st.Get(ctx, SettingsKey)
	if errors.KindOf(err) == errors.KindNotFound {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(ent.Value, &s); err != nil {
		return s, errors.Wrap(err, errors.KindInternal, "vip", "corrupt network settings")
	}
	return s, nil
}

// SaveSettings writes the network settings; zero settings delete the record.
func SaveSettings(ctx context.Context, st store.Store, s Settings) error {
	if s == (Settings{}) {
		err := st.Delete(ctx, SettingsKey, 0)
		if errors.KindOf(err) == errors.KindNotFound {
			return nil
		}
		return err
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = st.Put(ctx, SettingsKey, raw)
	return err
}

// EffectivePool is the pool in use: the cluster setting, else the node's configured fallback.
func EffectivePool(s Settings, fallback string) string {
	if strings.TrimSpace(s.ExternalPool) != "" {
		return s.ExternalPool
	}
	return fallback
}

// CheckPool rejects a pool that would not work on the LAN the nodes are on: each address must
// share a subnet with a node, and must not be a node's own, the subnet's network or broadcast
// address, or inside the cluster's own ranges. With no known node addresses only syntax is checked.
func CheckPool(pool string, nodes []netip.Addr) error {
	type item struct {
		lo, hi netip.Addr
		mask   int
	}
	var items []item
	n := 0
	for _, raw := range strings.Split(pool, ",") {
		if raw = strings.TrimSpace(raw); raw == "" {
			continue
		}
		lo, hi, mask, err := poolRange(raw)
		if err != nil {
			return err
		}
		for a := lo; n <= MaxPoolSize; a = a.Next() {
			if n++; a == hi || !a.Next().IsValid() {
				break
			}
		}
		if n > MaxPoolSize {
			return fmt.Errorf("the range holds more than %d addresses", MaxPoolSize)
		}
		items = append(items, item{lo, hi, mask})
	}
	for _, it := range items {
		for a := it.lo; ; a = a.Next() {
			if err := checkAddr(a, it.mask, nodes); err != nil {
				return err
			}
			if a == it.hi || !a.Next().IsValid() {
				break
			}
		}
	}
	return nil
}

func checkAddr(a netip.Addr, mask int, nodes []netip.Addr) error {
	for _, cidr := range []string{addrplan.OverlayCIDR, addrplan.InternalVIPCIDR, addrplan.BlockInternalCIDR} {
		if netip.MustParsePrefix(cidr).Contains(a) {
			return fmt.Errorf("%s is inside the cluster's own range %s", a, cidr)
		}
	}
	subnet := netip.PrefixFrom(a, mask).Masked()
	if a.Is4() && mask < 31 {
		if a == subnet.Addr() {
			return fmt.Errorf("%s is the network address of %s", a, subnet)
		}
		if a == lastAddr(subnet) {
			return fmt.Errorf("%s is the broadcast address of %s", a, subnet)
		}
	}
	if len(nodes) == 0 {
		return nil
	}
	onSubnet := false
	for _, nd := range nodes {
		if nd == a {
			return fmt.Errorf("%s is a node's address", a)
		}
		onSubnet = onSubnet || subnet.Contains(nd)
	}
	if !onSubnet {
		return fmt.Errorf("no node is on %s, so %s would not be reachable", subnet, a)
	}
	return nil
}

func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]) | host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// Assign allocates blockRef's address from pool (the LAN) when it is set, else from the internal
// range, and releases its record in the other scope so the block has one address.
func Assign(ctx context.Context, st store.Store, blockRef string, pool []netip.Prefix) (netip.Prefix, Scope, error) {
	scope, other := ScopeExternal, ScopeInternal
	if len(pool) == 0 {
		scope, other, pool = ScopeInternal, ScopeExternal, InternalPool()
	}
	p, err := Allocate(ctx, st, blockRef, scope, pool)
	if err != nil {
		return p, scope, err
	}
	// Read first: this runs every scan, and a delete of nothing would still be a Raft write.
	key := PoolKey(other, blockRef)
	ent, err := st.Get(ctx, key)
	if errors.KindOf(err) == errors.KindNotFound {
		return p, scope, nil
	}
	if err != nil {
		return p, scope, err
	}
	if err := st.Delete(ctx, key, ent.Revision); err != nil && errors.KindOf(err) != errors.KindNotFound {
		return p, scope, err
	}
	return p, scope, nil
}

// Lookup returns blockRef's current address, preferring the LAN one as DNS does.
func Lookup(ctx context.Context, st store.Store, blockRef string) (netip.Prefix, Scope, bool) {
	for _, scope := range []Scope{ScopeExternal, ScopeInternal} {
		ent, err := st.Get(ctx, PoolKey(scope, blockRef))
		if err != nil {
			continue
		}
		var a allocation
		if json.Unmarshal(ent.Value, &a) != nil {
			continue
		}
		if p, err := netip.ParsePrefix(a.Addr); err == nil {
			return p, scope, true
		}
	}
	return netip.Prefix{}, "", false
}
