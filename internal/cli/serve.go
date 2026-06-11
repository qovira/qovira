package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the Qovira HTTP server",
		Long:  "Start the Qovira HTTP API server (the Docker entrypoint for the application).",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// Stub: resolved flags will be wired to internal/app in QOV-50.
			return errors.New("serve: not yet implemented")
		},
	}

	// Reserve the --config flag so later slices can populate it without
	// breaking the flag surface.
	cmd.Flags().String("config", "", "path to config file (default: auto-discovered)")

	return cmd
}
