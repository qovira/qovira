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

// Process exit codes returned by Execute and handed to os.Exit by main. Specific failure modes can claim
// their own dedicated codes here as they emerge; for now any error collapses to exitError.
const (
	exitSuccess = 0
	exitError   = 1
)

// Execute is the public entry point. It builds the command tree, sets up a signal-aware context, and runs the
// root command. It returns the process exit code: exitSuccess on success, non-zero for any error.
func Execute() int {
	return ExecuteArgsWithOutput(os.Args[1:], os.Stdout, os.Stderr)
}

// ExecuteArgsWithOutput is the fully testable variant: args and the normal/error output writers are all
// configurable, so tests can capture help text and error output without a subprocess. Normal output (help,
// usage) goes to out; errors go to errOut.
func ExecuteArgsWithOutput(args []string, out, errOut io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	root := buildRoot(out, errOut)
	root.SetArgs(args)

	// SilenceErrors keeps cobra from printing the error itself, so the caller owns surfacing it. Print to
	// errOut here rather than letting it vanish — this covers flag-parsing errors, unknown commands, and any
	// RunE failure that returns before app.Run's logger is wired up (e.g. config loading).
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(errOut, "Error:", err)
		return exitError
	}

	return exitSuccess
}

// buildRoot constructs the cobra root command and attaches all subcommands.
func buildRoot(out, errOut io.Writer) *cobra.Command {
	var addr, logLevel, logFormat string

	root := &cobra.Command{
		Use:   "qovira",
		Short: "Qovira — self-hostable personal AI assistant",
		Long: `Qovira is a private, self-hostable AI personal assistant.
It serves a JSON/SSE API and an embedded web client backed by an encrypted store.`,
		SilenceErrors: true,
		SilenceUsage:  true,
	}

	root.SetOut(out)
	root.SetErr(errOut)

	// Persistent flags inherited by every subcommand.
	root.PersistentFlags().StringVar(&addr, "addr", app.DefaultAddr, "TCP listen/probe address, e.g. :18888 (env: QOVIRA_ADDR)")
	root.PersistentFlags().StringVar(&logLevel, "log-level", app.DefaultLogLevel, "log level: debug, info, warn, error (env: QOVIRA_LOG_LEVEL)")
	root.PersistentFlags().StringVar(&logFormat, "log-format", app.DefaultLogFormat, "log format: json, text (env: QOVIRA_LOG_FORMAT)")

	root.AddCommand(newServeCmd(&addr, &logLevel, &logFormat))
	root.AddCommand(newHealthcheckCmd(&addr))

	return root
}
