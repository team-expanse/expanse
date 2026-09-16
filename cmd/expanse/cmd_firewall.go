package main

import (
	"fmt"
	"net/netip"
	"strconv"

	"github.com/google/nftables"
	"github.com/spf13/cobra"

	fw "github.com/expanse/expanse/internal/network/firewall"
)

// newFirewallCmd provides `expanse ctl firewall show|test <port>`
// diagnostics (§4.5). Both read the live kernel ruleset and require
// root (CAP_NET_ADMIN) on the node.
func newFirewallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "firewall",
		Short: "Inspect the nftables firewall ruleset",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print the effective ruleset with live set contents",
		RunE: func(cmd *cobra.Command, args []string) error {
			c := &nftables.Conn{}
			t := &nftables.Table{Family: nftables.TableFamilyINet, Name: fw.Table}
			d := fw.Desired{}
			for _, def := range []struct {
				name  string
				v4    bool
				tgt   func(addrs []netip.Addr)
				ports func(ports []uint16)
			}{{
				name: fw.SetPeers, v4: true, tgt: func(a []netip.Addr) { d.Peers = a },
			}, {
				name: fw.SetVIPs, v4: true, tgt: func(a []netip.Addr) {
					for _, x := range a {
						d.VIPs = append(d.VIPs, netip.PrefixFrom(x, 32))
					}
				},
			}, {
				name: fw.SetTCPPorts, ports: func(p []uint16) { d.TCPPorts = p },
			}, {
				name: fw.SetUDPPorts, ports: func(p []uint16) { d.UDPPorts = p },
			}} {
				set, err := c.GetSetByName(t, def.name)
				if err != nil {
					return fmt.Errorf("firewall: %s not loaded (agent bootstrapped?): %w", def.name, err)
				}
				elems, err := c.GetSetElements(set)
				if err != nil {
					return fmt.Errorf("firewall: read %s: %w", def.name, err)
				}
				var addrs []netip.Addr
				var ports []uint16
				for _, e := range elems {
					if def.v4 && len(e.Key) == 4 {
						addrs = append(addrs, netip.AddrFrom4([4]byte(e.Key)))
					} else if !def.v4 && len(e.Key) == 2 {
						ports = append(ports, uint16(e.Key[0])<<8|uint16(e.Key[1]))
					}
				}
				if def.tgt != nil {
					def.tgt(addrs)
				}
				if def.ports != nil {
					def.ports(ports)
				}
			}
			fmt.Fprint(cmd.OutOrStdout(), fw.Render(d))
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "test <port>",
		Short: "Check whether a port is admitted by the per-block sets",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			port, err := strconv.ParseUint(args[0], 10, 16)
			if err != nil {
				return fmt.Errorf("port %q: %w", args[0], err)
			}
			c := &nftables.Conn{}
			tcp, udp, vip, err := fw.Membership(c, uint16(port))
			if err != nil {
				return fmt.Errorf("firewall: not loaded (agent bootstrapped?): %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "port %d: block_tcp=%t block_udp=%t vip_addresses_populated=%t\n",
				port, tcp, udp, vip)
			fmt.Fprintln(cmd.OutOrStdout(), "note: admission additionally requires the destination to be a VIP address (ip daddr @vip_addresses)")
			return nil
		},
	})
	return cmd
}
