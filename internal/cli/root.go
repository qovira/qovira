// Package cli builds the Cobra command tree for the Qovira CLI. Commands are thin wrappers that delegate all
// logic to internal packages; no business logic belongs here.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/qovira/qovira/internal/app"
)

// Execute is the public entry point. It builds the command tree, sets up a signal-aware context, and runs the
// root command. It returns the process exit code: 0 for success, non-zero for any error.
func Execute() int {
	return ExecuteArgsWithOutput(os.Args[1:], os.Stdout)
}

// ExecuteArgsWithOutput is the fully testable variant: args and output writer are both configurable, so tests
// can capture help text without a subprocess.
func ExecuteArgsWithOutput(args []string, out io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	root := buildRoot(out)
	root.SetArgs(args)

	if err := root.ExecuteContext(ctx); err != nil {
		return 1
	}

	return 0
}

// buildRoot constructs the cobra root command and attaches all subcommands.
func buildRoot(out io.Writer) *cobra.Command {
	var logLevel, logFormat string

	root := &cobra.Command{
		Use:   "qovira",
		Short: "Qovira — self-hostable personal AI assistant",
		Long: `Qovira is a private, self-hostable AI personal assistant.
It serves a JSON/SSE API and an embedded web client backed by an encrypted SQLite store.`,
		SilenceErrors: true,
		SilenceUsage:  true,
	}

	root.SetOut(out)
	root.SetErr(out)

	// Persistent flags inherited by every subcommand.
	root.PersistentFlags().StringVar(&logLevel, "log-level", "", "log level: debug, info, warn, error (default: info, env: QOVIRA_LOG_LEVEL)")
	root.PersistentFlags().StringVar(&logFormat, "log-format", "", "log format: json, text (default: json, env: QOVIRA_LOG_FORMAT)")

	root.AddCommand(newServeCmd(&logLevel, &logFormat))
	root.AddCommand(newHealthcheckCmd())

	return root
}

// newServeCmd returns the `qovira serve` subcommand.
func newServeCmd(logLevel, logFormat *string) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the HTTP server",
		Long:  "Start the Qovira HTTP server and block until a shutdown signal is received.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var overrides app.FlagOverrides

			// cmd.Flags() exposes the inherited persistent flags once cobra has parsed them, so the
			// parent command is not needed here to read whether they were explicitly set.
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
