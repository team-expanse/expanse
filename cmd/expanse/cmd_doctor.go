package main

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/expanse/expanse/internal/doctor"
)

// newDoctorCmd provides `expanse doctor network` (§5): 12 live checks
// with a PASS/WARN/FAIL table and a remediation hint per row. Peer/
// VIP/block targets come from flags (the VM test passes the known
// cluster shape); omitted targets degrade the corresponding rows to
// WARN rather than failing the run.
func newDoctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose node and cluster health",
	}
	var (
		peersFl  []string // node=10.42.N.1[,port[,port...]]
		vipsFl   []string
		blocksFl []string
	)
	network := &cobra.Command{
		Use:   "network",
		Short: "Run the 12 §5 networking checks",
		RunE: func(cmd *cobra.Command, args []string) error {
			var peers []doctor.Peer
			for _, p := range peersFl {
				f := strings.SplitN(p, "=", 2)
				if len(f) != 2 {
					return fmt.Errorf("peer must be node=overlay[,port...], got %q", p)
				}
				peer := doctor.Peer{Node: f[0]}
				parts := strings.Split(f[1], ",")
				if a, err := netip.ParseAddr(parts[0]); err != nil {
					return fmt.Errorf("peer %s: %w", p, err)
				} else {
					peer.Overlay = a
				}
				for _, ps := range parts[1:] {
					port, err := strconv.Atoi(ps)
					if err != nil {
						return fmt.Errorf("peer %s: bad port %q", p, ps)
					}
					peer.ClusterPorts = append(peer.ClusterPorts, port)
				}
				peers = append(peers, peer)
			}
			var vips []netip.Addr
			for _, v := range vipsFl {
				a, err := netip.ParseAddr(v)
				if err != nil {
					return fmt.Errorf("vip %q: %w", v, err)
				}
				vips = append(vips, a)
			}
			in := doctor.CollectLive(cmd.Context(), peers, vips, blocksFl)
			rs := doctor.Run(in)
			fmt.Fprint(cmd.OutOrStdout(), doctor.Format(rs))
			for _, r := range rs {
				if r.Status == doctor.Fail {
					os.Exit(1)
				}
			}
			return nil
		},
	}
	network.Flags().StringArrayVar(&peersFl, "peer", nil,
		"cluster peer as node=overlay[,port...] (repeatable)")
	network.Flags().StringSliceVar(&vipsFl, "vip", nil, "VIP address to check (repeatable)")
	network.Flags().StringSliceVar(&blocksFl, "block", nil, "block DNS name to resolve (repeatable)")
	cmd.AddCommand(network)
	cmd.AddCommand(newDoctorStorageCmd())
	return cmd
}

// newDoctorStorageCmd provides `expanse doctor storage` (A5): the DRBD
// module and version, the volume group's headroom, thin pool exhaustion,
// the system volume's mirror state, and every configured DRBD resource.
func newDoctorStorageCmd() *cobra.Command {
	var vg, systemMount string
	cmd := &cobra.Command{
		Use:   "storage",
		Short: "Check DRBD, LVM and the system volume",
		RunE: func(cmd *cobra.Command, args []string) error {
			in := doctor.CollectStorageLive(cmd.Context(), vg, systemMount)
			rs := doctor.RunStorage(in)
			fmt.Fprint(cmd.OutOrStdout(), doctor.Format(rs))
			for _, r := range rs {
				if r.Status == doctor.Fail {
					os.Exit(1)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&vg, "vg", "", "LVM volume group to check (matches expansed's --storage-vg); empty skips VG/pool checks")
	cmd.Flags().StringVar(&systemMount, "system-mount", "/", "mountpoint of the system btrfs volume")
	return cmd
}
