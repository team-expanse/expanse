package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/expanse/expanse/internal/version"
)

func newVersionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		RunE:  runVersion,
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

// runVersion prints internal/version, the one version the package build stamps (the web UI shows it too).
func runVersion(cmd *cobra.Command, args []string) error {
	info := version.Get()
	if jsonFlag, _ := cmd.Flags().GetBool("json"); jsonFlag {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(info)
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), info)
	return err
}
