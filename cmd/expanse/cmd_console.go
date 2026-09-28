package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/expanse/expanse/internal/console"
	"github.com/expanse/expanse/internal/tuikit"
)

func newConsoleCmd() *cobra.Command {
	c := &console.Collector{Options: console.Options{Socket: defaultSocket, DataDir: defaultPersistDir, Timeout: 2 * time.Second}}
	opts := &c.Options
	var interval time.Duration
	var once bool
	cmd := &cobra.Command{
		Use:   "console",
		Short: "Full-screen host information screen for tty1 (read-only; expanse-console.service)",
		Long: `console shows the node's identity, addresses, web UI URL, cluster membership,
health and hardware on the terminal, refreshing every few seconds. It is read-only:
no shell, no actions, no secrets. The node module runs it on tty1 in place of a login.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if once {
				lines := console.Render(c.Collect(cmd.Context()), tuikit.VT, tuikit.DefaultGlyphs())
				fmt.Fprintln(cmd.OutOrStdout(), strings.Join(lines, "\n"))
				return nil
			}
			return runConsole(cmd.Context(), c, interval)
		},
	}
	cmd.Flags().StringVar(&opts.Socket, "socket", defaultSocket, "agent unix socket path")
	cmd.Flags().StringVar(&opts.DataDir, "data-dir", defaultPersistDir, "persistent state directory (node id, identity)")
	cmd.Flags().DurationVar(&interval, "interval", 3*time.Second, "refresh interval")
	cmd.Flags().BoolVar(&once, "once", false, "print one 80x25 screen to stdout and exit")
	return cmd
}

// runConsole repaints the screen every interval and on resize until the context ends
// (SIGTERM from systemd) or the tty goes away; keys are read and ignored.
func runConsole(ctx context.Context, c *console.Collector, interval time.Duration) error {
	term, err := tuikit.Open()
	if err != nil {
		return fmt.Errorf("open terminal: %w", err)
	}
	defer term.Close()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	info := c.Collect(ctx)
	for {
		term.Draw(console.Render(info, term.Size(), term.Glyphs()))
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			info = c.Collect(ctx)
		case <-term.Resized():
		case _, ok := <-term.Keys():
			if !ok {
				return nil
			}
		}
	}
}
