package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/qovira/qovira/internal/app"
)

// newServeCmd returns the `qovira serve` subcommand.
func newServeCmd(addr, logLevel, logFormat *string) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the HTTP server",
		Long:  "Start the Qovira HTTP server and block until a shutdown signal is received.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var overrides app.FlagOverrides

			if cmd.Flags().Changed("addr") {
				overrides.Addr = addr
			}

			if cmd.Flags().Changed("log-level") {
				overrides.LogLevel = logLevel
			}

			if cmd.Flags().Changed("log-format") {
				overrides.LogFormat = logFormat
			}

			cfg, err := app.LoadConfig(overrides)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			return app.Run(cmd.Context(), cfg)
		},
	}
}
