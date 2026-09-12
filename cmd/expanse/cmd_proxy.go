package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newProxyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "proxy",
		Short: "Manage proxy",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(os.Stderr, "not implemented")
			return fmt.Errorf("not implemented")
		},
	}
}
