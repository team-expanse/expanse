package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newAgentCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "agent",
		Short: "Manage agents",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(os.Stderr, "not implemented")
			return fmt.Errorf("not implemented")
		},
	}
}
