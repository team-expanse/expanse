package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/expanse/expanse/internal/agent"
)

func newAgentCmd() *cobra.Command {
	var (
		dataDir    string
		socket     string
		period     string
		ctlPeriod  string
		dryRun     bool
		enableTCP  bool
		role       string
		raftBind   string
		raftAdv    string
		blockCat   string
		blockFlake string
		extPool    string
		extIface   string
	)
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run the node agent (expansed)",
		RunE: func(cmd *cobra.Command, args []string) error {
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
	cmd.Flags().StringVar(&raftAdv, "raft-advertise", "", "raft transport advertised addr (default: the bind host or local IP, port 7444)")
	cmd.Flags().StringVar(&blockCat, "blocks-catalog", "", "shipped block-type directory (nix/blocks layout); empty = block API disabled")
	cmd.Flags().StringVar(&blockFlake, "blocks-flake-ref", "", "flake ref holding block closures (attr per type: <category>-<name>); empty = replicas not realized on this node")
	return cmd
}
