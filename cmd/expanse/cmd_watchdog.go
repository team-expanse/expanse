package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"

	"github.com/expanse/expanse/internal/agent/nix"
)

// newWatchdogCmd implements expanse-switch-watchdog: the safety net that
// makes remote config changes survivable (D2.6). Run periodically by
// expanse-switch-watchdog.timer; if the agent died mid-switch (stale
// /persist/expanse/pending-switch marker), it rolls back to the previous
// system generation and reboots.
func newWatchdogCmd() *cobra.Command {
	var (
		marker   string
		stale    time.Duration
		rollback bool
	)
	cmd := &cobra.Command{
		Use:   "watchdog",
		Short: "Switch watchdog: roll back and reboot if a switch was left incomplete",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWatchdog(cmd, marker, stale, rollback)
		},
	}
	cmd.Flags().StringVar(&marker, "marker", "/persist/expanse/pending-switch", "pending-switch marker path")
	cmd.Flags().DurationVar(&stale, "stale", 10*time.Minute, "age at which a pending switch is considered failed")
	cmd.Flags().BoolVar(&rollback, "rollback", true, "roll back to the previous generation before rebooting")
	return cmd
}

func runWatchdog(cmd *cobra.Command, marker string, stale time.Duration, rollback bool) error {
	st, err := os.Stat(marker)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no pending switch: healthy
		}
		return fmt.Errorf("stat %s: %w", marker, err)
	}
	age := time.Since(st.ModTime())
	if age < stale {
		return nil // switch in progress: fine
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "stale pending-switch marker (%s old); recovering\n", age.Round(time.Second))
	if rollback {
		d := nix.New()
		d.PendingSwitchPath = marker
		if err := d.Rollback(cmd.Context(), 0); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "rollback failed: %v (rebooting anyway)\n", err)
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "rolled back to previous generation\n")
		}
		// The rollback itself switched; clear the marker so we don't loop.
		_ = os.Remove(marker)
	}
	// A reboot brings all system state in line with the rolled-back config.
	if err := exec.Command("systemctl", "reboot").Start(); err != nil {
		return fmt.Errorf("reboot: %w", err)
	}
	return nil
}
