package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newUICmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ui",
		Short: "Manage UI components",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(os.Stderr, "not implemented")
			return fmt.Errorf("not implemented")
		},
	}
}
