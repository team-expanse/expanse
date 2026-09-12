package main

import (
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"
)

var (
	buildVersion = "dev"
	buildCommit  = "none"
	buildDate    = "unknown"
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

func runVersion(cmd *cobra.Command, args []string) error {
	jsonFlag, _ := cmd.Flags().GetBool("json")
	if jsonFlag {
		fmt.Fprintf(os.Stdout, `{"version":"%s","commit":"%s","date":"%s","go_version":"%s","platform":"%s/%s"}
`, buildVersion, buildCommit, buildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return nil
	}
	fmt.Fprintf(os.Stdout, "expanse %s (%s) %s/%s %s\n", buildVersion, buildCommit, runtime.GOOS, runtime.GOARCH, runtime.Version())
	return nil
}