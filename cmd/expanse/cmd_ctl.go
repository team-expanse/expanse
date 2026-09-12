package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newCtlCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ctl",
		Short: "Management control commands",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(os.Stderr, "not implemented")
			return fmt.Errorf("not implemented")
		},
	}
}
