package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Build-info variables injected via -ldflags -X at compile time. Defaults to human-readable sentinel values so the
// binary is still usable without a full release build (e.g. during local development).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version and build information",
		Long:  "Print the qovira version string, git commit, and build date.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "qovira %s (commit %s, built %s)\n", version, commit, date)
			return nil
		},
	}
}
