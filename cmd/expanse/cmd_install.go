package main

import (
	"fmt"
	"os"

	"github.com/expanse/expanse/internal/install"
	"github.com/spf13/cobra"
)

func newInstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install Expanse onto this machine's disks",
		Long: `install turns the current machine (booted from the Expanse installer
ISO or any Linux with the prerequisites) into an Expanse node.

Stages: preflight, detect, confirm, partition (disko), snapshot,
identity, config, install (nixos-install), verify.`,
		RunE: runInstall,
	}
	cmd.Flags().String("config", "", "install config yaml (omit for interactive TUI)")
	cmd.Flags().Bool("dry-run", false, "print the plan and every command it would run; touch nothing")
	cmd.Flags().Bool("force", false, "allow wiping non-empty disks")
	cmd.Flags().String("target-flake", "", "flake to install from (default: $EXPANSE_FLAKE or /run/expanse-flake)")
	cmd.Flags().Bool("tui", false, "launch the interactive installer TUI")
	cmd.Flags().Bool("skip-system-install", false, "run all stages except nixos-install (testing)")
	return cmd
}

func runInstall(cmd *cobra.Command, args []string) error {
	configPath, _ := cmd.Flags().GetString("config")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	force, _ := cmd.Flags().GetBool("force")
	targetFlake, _ := cmd.Flags().GetString("target-flake")
	tui, _ := cmd.Flags().GetBool("tui")

	if targetFlake == "" {
		if v := os.Getenv("EXPANSE_FLAKE"); v != "" {
			targetFlake = v
		}
	}
	if tui || (configPath == "" && !dryRun) {
		return runTUI()
	}

	if configPath == "" && dryRun {
		return fmt.Errorf("--dry-run requires --config (no assumptions about target disks)")
	}

	skip, _ := cmd.Flags().GetBool("skip-system-install")

	return install.Run(install.Options{
		ConfigPath:        configPath,
		DryRun:            dryRun,
		Force:             force,
		TargetFlake:       targetFlake,
		SkipSystemInstall: skip,
		Logger: func(format string, a ...any) {
			fmt.Fprintf(cmd.OutOrStderr(), format+"\n", a...)
		},
	})
}
