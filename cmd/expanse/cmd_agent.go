package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/expanse/expanse/internal/agent"
)

func newAgentCmd() *cobra.Command {
	var (
		dataDir   string
		socket    string
		period    string
		dryRun    bool
		enableTCP bool
		role      string
		raftBind  string
		raftAdv   string
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
				LogLevel:          cmd.Root().PersistentFlags().Lookup("log-level").Value.String(),
			}
			if period != "" {
				d, err := parseDuration(period)
				if err != nil {
					return fmt.Errorf("--period: %w", err)
				}
				cfg.Period = d
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
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "plan but never apply changes")
	cmd.Flags().BoolVar(&enableTCP, "enable-tcp", false, "enable the TCP gRPC listener (no mTLS yet; off by default)")
	cmd.Flags().StringVar(&role, "role", "", "cluster role override (§4.9: voter | witness; default: from the node record)")
	cmd.Flags().StringVar(&raftBind, "raft-bind", "", "raft transport bind addr (default 0.0.0.0:7444; must match the join-time bind)")
	cmd.Flags().StringVar(&raftAdv, "raft-advertise", "", "raft transport advertised addr (default: the bind host or local IP, port 7444)")
	return cmd
}
