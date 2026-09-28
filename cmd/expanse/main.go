package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/expanse/expanse/internal/logging"

	"github.com/spf13/cobra"
)

type rootCmd struct {
	cmd       *cobra.Command
	ctx       context.Context
	logLevel  string
	logFormat string
	logger    *slog.Logger
}

func newRootCmd(ctx context.Context) *rootCmd {
	rc := &rootCmd{ctx: ctx, logLevel: "info", logFormat: "text"}
	rc.cmd = &cobra.Command{
		Use:   "expanse",
		Short: "Expanse server infrastructure CLI",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			opts := logging.Options{Level: rc.logLevel, Format: rc.logFormat, Output: os.Stdout}
			l, err := logging.Setup(opts)
			if err != nil {
				return fmt.Errorf("setup logger: %w", err)
			}
			rc.logger = l
			return nil
		},
	}
	rc.cmd.PersistentFlags().StringVar(&rc.logLevel, "log-level", "info", "log level (debug|info|warn|error)")
	rc.cmd.PersistentFlags().StringVar(&rc.logFormat, "log-format", "text", "log format (text|json)")
	rc.cmd.AddCommand(newAgentCmd())
	rc.cmd.AddCommand(newUICmd())
	rc.cmd.AddCommand(newProxyCmd())
	rc.cmd.AddCommand(newCtlCmd())
	rc.cmd.AddCommand(newFirewallCmd())
	rc.cmd.AddCommand(newClusterCmd())
	rc.cmd.AddCommand(newDoctorCmd())
	rc.cmd.AddCommand(newVersionCmd())
	rc.cmd.AddCommand(newInstallCmd())
	rc.cmd.AddCommand(newNodeCmd())
	rc.cmd.AddCommand(newWatchdogCmd())
	rc.cmd.AddCommand(newConsoleCmd())
	return rc
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rc := newRootCmd(ctx)
	if err := rc.cmd.ExecuteContext(ctx); err != nil {
		logging.Default().Error("fatal", "err", err)
		os.Exit(1)
	}
}
