package main

import (
	"fmt"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* on http.DefaultServeMux, served only when --pprof-addr is set
	"os"

	"github.com/spf13/cobra"

	"github.com/expanse/expanse/internal/agent"
)

func newAgentCmd() *cobra.Command {
	var (
		dataDir     string
		socket      string
		period      string
		ctlPeriod   string
		dryRun      bool
		enableTCP   bool
		role        string
		raftBind    string
		raftAdv     string
		blockCat    string
		blockFlake  string
		extPool     string
		extIface    string
		dnsUp       string
		storageVG   string
		storagePool string
		drbdCfgDir  string
		lostAfter   string
		firewall    bool
		pprofAddr   string
	)
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run the node agent (expansed)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if pprofAddr != "" {
				go func() {
					if err := http.ListenAndServe(pprofAddr, nil); err != nil {
						fmt.Fprintf(os.Stderr, "pprof listener on %s failed: %v\n", pprofAddr, err)
					}
				}()
			}
			cfg := agent.Config{
				DataDir:           dataDir,
				Socket:            socket,
				DryRun:            dryRun,
				EnableTCP:         enableTCP,
				Role:              role,
				RaftBindAddr:      raftBind,
				RaftAdvertiseAddr: raftAdv,
				BlocksCatalog:     blockCat,
				BlocksFlakeRef:    blockFlake,
				ExternalVIPPool:   extPool,
				ExternalInterface: extIface,
				DNSUpstreams:      dnsUp,
				StorageVG:         storageVG,
				StoragePool:       storagePool,
				DRBDConfigDir:     drbdCfgDir,
				Firewall:          firewall,
				LogLevel:          cmd.Root().PersistentFlags().Lookup("log-level").Value.String(),
			}
			if period != "" {
				d, err := parseDuration(period)
				if err != nil {
					return fmt.Errorf("--period: %w", err)
				}
				cfg.Period = d
			}
			if ctlPeriod != "" {
				d, err := parseDuration(ctlPeriod)
				if err != nil {
					return fmt.Errorf("--controller-period: %w", err)
				}
				cfg.ControllerPeriod = d
			}
			if lostAfter != "" {
				d, err := parseDuration(lostAfter)
				if err != nil {
					return fmt.Errorf("--storage-lost-after: %w", err)
				}
				cfg.StorageLostAfter = d
			}
			a, err := agent.New(cfg)
			if err != nil {
				return fmt.Errorf("init agent: %w", err)
			}
			return a.Run(cmd.Context())
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "/persist/expanse", "persistent state directory")
	cmd.Flags().StringVar(&socket, "socket", "/run/expanse/agent.sock", "gRPC unix socket path")
	cmd.Flags().StringVar(&period, "period", "30s", "reconcile tick period")
	cmd.Flags().StringVar(&ctlPeriod, "controller-period", "", "block placement controller pass interval (default 30s)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "plan but never apply changes")
	cmd.Flags().BoolVar(&enableTCP, "enable-tcp", false, "enable the TCP gRPC listener (no mTLS yet; off by default)")
	cmd.Flags().StringVar(&role, "role", "", "cluster role override (§4.9: voter | witness; default: from the node record)")
	cmd.Flags().StringVar(&raftBind, "raft-bind", "", "raft transport bind addr (default 0.0.0.0:7444; must match the join-time bind)")
	cmd.Flags().StringVar(&extPool, "external-vip-pool", os.Getenv("EXPANSE_EXTERNAL_VIP_POOL"),
		"external VIP pool (§4.2), e.g. 192.168.1.100-192.168.1.120; empty = internal VIPs only")
	cmd.Flags().StringVar(&extIface, "external-interface", "",
		"physical interface to announce external VIPs on (default: auto = default route)")
	cmd.Flags().StringVar(&dnsUp, "dns-upstreams", "",
		"DNS forwarders (T17), comma-separated ip:port; empty = /etc/resolv.conf")
	cmd.Flags().StringVar(&storageVG, "storage-vg", "", "LVM volume group for volume replicas; empty = volume storage disabled")
	cmd.Flags().StringVar(&storagePool, "storage-pool", "", "thin pool inside --storage-vg; empty = thick volumes")
	cmd.Flags().StringVar(&lostAfter, "storage-lost-after", "", "how long a node stays gone before its volume replicas are rebuilt elsewhere (default 10m)")
	cmd.Flags().StringVar(&drbdCfgDir, "drbd-config-dir", "", "directory for DRBD resource files (default /etc/drbd.d)")
	cmd.Flags().BoolVar(&firewall, "firewall", false,
		"apply the §4.5 nftables ruleset (static skeleton + store-driven dynamic sets)")
	cmd.Flags().StringVar(&raftAdv, "raft-advertise", "", "raft transport advertised addr (default: the bind host or local IP, port 7444)")
	cmd.Flags().StringVar(&blockCat, "blocks-catalog", "", "shipped block-type directory (nix/blocks layout); empty = block API disabled")
	cmd.Flags().StringVar(&blockFlake, "blocks-flake-ref", "", "flake ref holding block closures (attr per type: <category>-<name>); empty = replicas not realized on this node")
	cmd.Flags().StringVar(&pprofAddr, "pprof-addr", "", "serve net/http/pprof on this addr (e.g. 127.0.0.1:6060); empty = disabled (debug only, no auth)")
	return cmd
}
