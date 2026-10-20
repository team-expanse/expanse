package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/expanse/expanse/internal/agent/nix"
	"github.com/expanse/expanse/internal/upgrade"
)

const upgradeUnit = "expanse-upgrade"

func newUpgradeCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "upgrade", Short: "Move this node to another Expanse release"}
	cmd.AddCommand(newUpgradeStartCmd(), newUpgradeApplyCmd(), newUpgradeStatusCmd(), newUpgradeSignCmd())
	return cmd
}

// newUpgradeStartCmd runs apply as its own unit: the switch restarts expansed, and with it its children.
func newUpgradeStartCmd() *cobra.Command {
	var baseURL, key string
	cmd := &cobra.Command{
		Use:   "start VERSION",
		Short: "Upgrade this node in the background (unit " + upgradeUnit + ")",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			self, err := os.Executable()
			if err != nil {
				return err
			}
			run := []string{
				"--unit=" + upgradeUnit, "--collect", "--property=Type=exec",
				"--setenv=PATH=/run/current-system/sw/bin", self, "upgrade", "apply", args[0], "--base-url", baseURL,
			}
			if key != "" {
				run = append(run, "--key", key)
			}
			out, err := exec.CommandContext(cmd.Context(), "systemd-run", run...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("systemd-run: %w: %s", err, out)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "upgrading to %s; follow it with journalctl -fu %s\n", args[0], upgradeUnit)
			return nil
		},
	}
	cmd.Flags().StringVar(&baseURL, "base-url", upgrade.DefaultBaseURL, "release download URL, one directory per tag")
	cmd.Flags().StringVar(&key, "key", "", "release public key (name:base64); default: this build's")
	return cmd
}

func newUpgradeApplyCmd() *cobra.Command {
	var nixosDir, cacheDir, baseURL, status, key string
	cmd := &cobra.Command{
		Use:   "apply VERSION",
		Short: "Fetch, verify, build and switch to a release (blocks until done)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if key == "" {
				key = upgrade.ReleaseKey()
			}
			if key == "" {
				return fmt.Errorf("this build has no release key to verify releases with")
			}
			u := &upgrade.Upgrader{
				Source:       &upgrade.Source{BaseURL: baseURL, PublicKey: key, CacheDir: cacheDir},
				Tree:         &upgrade.Tree{Dir: nixosDir},
				Nix:          &upgrade.ExecNix{PublicKey: key, Driver: nix.New()},
				StatusPath:   status,
				BootedSystem: "/run/booted-system",
			}
			return u.Apply(cmd.Context(), args[0], cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&nixosDir, "nixos-dir", "/etc/nixos", "the node's NixOS config, holding expanse/")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "/var/cache/expanse/upgrade", "where downloads are kept")
	cmd.Flags().StringVar(&baseURL, "base-url", upgrade.DefaultBaseURL, "release download URL, one directory per tag")
	cmd.Flags().StringVar(&status, "status", upgrade.DefaultStatusPath, "status file")
	cmd.Flags().StringVar(&key, "key", "", "release public key (name:base64); default: this build's")
	return cmd
}

func newUpgradeStatusCmd() *cobra.Command {
	var status string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show this node's last upgrade",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := upgrade.ReadStatus(status)
			if os.IsNotExist(err) {
				fmt.Fprintln(cmd.OutOrStdout(), "no upgrade has run on this node")
				return nil
			}
			if err != nil {
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(st)
		},
	}
	cmd.Flags().StringVar(&status, "status", upgrade.DefaultStatusPath, "status file")
	return cmd
}

// newUpgradeSignCmd writes SUMS.sig for release.sh (and the rollout VM test's release).
func newUpgradeSignCmd() *cobra.Command {
	var keyFile string
	cmd := &cobra.Command{
		Use:   "sign SHA256SUMS",
		Short: "Sign a release's SHA256SUMS with a Nix secret key, writing SHA256SUMS.sig",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			secret, err := os.ReadFile(keyFile)
			if err != nil {
				return err
			}
			sums, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			sig, err := upgrade.Sign(string(secret), sums)
			if err != nil {
				return err
			}
			return os.WriteFile(args[0]+".sig", sig, 0o644)
		},
	}
	cmd.Flags().StringVar(&keyFile, "key", "", "Nix secret key file (nix key generate-secret)")
	_ = cmd.MarkFlagRequired("key")
	return cmd
}
