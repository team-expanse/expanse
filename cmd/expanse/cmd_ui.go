package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/expanse/expanse/internal/config"
)

// newUICmd builds `expanse ui`. The web interface itself needs no
// subcommand to start: it is served automatically by every cluster
// node's `expanse agent` (ROADMAP.md Phase 2, D1 — in-process, not a
// separate binary). This command is an operator-facing pointer to it,
// not a control surface.
func newUICmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ui",
		Short: "Show how to reach the web management interface",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(cmd.OutOrStdout(),
				"The web UI is served automatically by every cluster node's agent on port %d (TLS, signed by the cluster CA).\nReach it at https://<any-node-or-the-UI-VIP-address>:%d/\n",
				config.PortUI, config.PortUI)
			return nil
		},
	}
}
