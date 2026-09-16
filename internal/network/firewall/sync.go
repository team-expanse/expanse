package firewall

import (
	"fmt"

	"github.com/google/nftables"
)

// currentKeys reads the live contents of one set as a key-set.
func currentKeys(c Conn, s *nftables.Set) (map[string]bool, error) {
	elems, err := c.GetSetElements(s)
	if err != nil {
		return nil, fmt.Errorf("firewall: get %s: %w", s.Name, err)
	}
	cur := make(map[string]bool, len(elems))
	for _, e := range elems {
		cur[string(e.Key)] = true
	}
	return cur, nil
}

// diffSet computes the element adds and deletes needed to move one set
// from its current contents to desired. Element add/delete only —
// never a set flush or ruleset replace (D5.6).
func diffSet(c Conn, s *nftables.Set, desired map[string]bool) (add, del []string, err error) {
	cur, err := currentKeys(c, s)
	if err != nil {
		return nil, nil, err
	}
	for k := range desired {
		if !cur[k] {
			add = append(add, k)
		}
	}
	for k := range cur {
		if !desired[k] {
			del = append(del, k)
		}
	}
	return add, del, nil
}

func elements(keys []string) []nftables.SetElement {
	out := make([]nftables.SetElement, 0, len(keys))
	for _, k := range keys {
		out = append(out, nftables.SetElement{Key: []byte(k)})
	}
	return out
}

// Sync moves all four dynamic sets to the desired contents using only
// SetAddElements / SetDeleteElements. Idempotent: with no changes it
// issues no kernel operations at all.
func Sync(c Conn, d Desired) error {
	want := d.normalize()
	t := &nftables.Table{Family: nftables.TableFamilyINet, Name: Table}
	plan := []struct {
		set     *nftables.Set
		desired map[string]bool
		setName string
	}{
		{set4(t, SetPeers), want.peers, SetPeers},
		{set4(t, SetVIPs), want.vips, SetVIPs},
		{set16(t, SetTCPPorts), want.tcp, SetTCPPorts},
		{set16(t, SetUDPPorts), want.udp, SetUDPPorts},
	}
	for _, p := range plan {
		add, del, err := diffSet(c, p.set, p.desired)
		if err != nil {
			return err
		}
		if len(add) > 0 {
			if err := c.SetAddElements(p.set, elements(add)); err != nil {
				return fmt.Errorf("firewall: add to %s: %w", p.setName, err)
			}
		}
		if len(del) > 0 {
			if err := c.SetDeleteElements(p.set, elements(del)); err != nil {
				return fmt.Errorf("firewall: delete from %s: %w", p.setName, err)
			}
		}
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("firewall: flush set updates: %w", err)
	}
	return nil
}

// Membership reports whether port is present in the live tcp/udp block
// port sets. Used by `expanse ctl firewall test <port>`.
func Membership(c Conn, port uint16) (tcp, udp, vip bool, err error) {
	t := &nftables.Table{Family: nftables.TableFamilyINet, Name: Table}
	key := portKey(port)
	for _, p := range []struct {
		name    string
		present *bool
	}{
		{SetTCPPorts, &tcp},
		{SetUDPPorts, &udp},
	} {
		cur, err := currentKeys(c, &nftables.Set{Table: t, Name: p.name})
		if err != nil {
			return false, false, false, err
		}
		*p.present = cur[key]
	}
	vips, err := currentKeys(c, &nftables.Set{Table: t, Name: SetVIPs})
	if err != nil {
		return false, false, false, err
	}
	vip = len(vips) > 0
	return tcp, udp, vip, nil
}
