package main

import (
	"fmt"

	"github.com/expanse/expanse/internal/install"
	"github.com/spf13/cobra"
)

func newNodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Node identity and local state management",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "init",
		Short: "Ensure node identity exists (idempotent; used by expanse-firstboot.service)",
		RunE:  runNodeInit,
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "info",
		Short: "Print this node's identity",
		RunE:  runNodeInfo,
	})
	return cmd
}

const defaultPersistDir = "/persist/expanse"

func runNodeInit(cmd *cobra.Command, args []string) error {
	id, err := install.EnsureIdentity(defaultPersistDir + "/" + install.IdentityDir)
	if err != nil {
		return fmt.Errorf("ensure identity: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "node-id %s\n", id.NodeID)
	return nil
}

func runNodeInfo(cmd *cobra.Command, args []string) error {
	id, err := install.LoadIdentity(defaultPersistDir + "/" + install.IdentityDir)
	if err != nil {
		return fmt.Errorf("load identity: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "node-id:    %s\n", id.NodeID)
	fmt.Fprintf(cmd.OutOrStdout(), "public-key: %x\n", id.PublicKey)
	fmt.Fprintf(cmd.OutOrStdout(), "created:    %s\n", id.Created)
	return nil
}
