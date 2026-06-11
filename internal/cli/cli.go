// Package cli is the Cobra command tree for qovira (serve, migrate, version).
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Execute builds the root command tree and runs it. It returns a process exit code (0 for success, non-zero for error) suitable for passing directly to os.Exit.
func Execute() int {
	return execute(newRootCmd())
}

// execute runs an already-constructed root command and maps the outcome to a process exit code. On error it prints a single diagnostic to the command's error stream — the root sets SilenceErrors, so Cobra does not also print it (avoiding a double message). Split out from Execute so tests can inject a root configured with SetArgs/SetErr and assert the printed diagnostic.
func execute(root *cobra.Command) int {
	if err := root.Execute(); err != nil {
		fmt.Fprintln(root.ErrOrStderr(), "qovira:", err)
		return 1
	}
	return 0
}

// newRootCmd constructs the full command tree. Tests call this directly so they can configure SetArgs/SetOut/SetErr.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "qovira",
		Short: "A private, self-hostable personal assistant",
		Long: `Qovira is a personal assistant you run yourself — your reminders, notes,
calendar, and quick answers, organized by AI on a server you own and a model
you choose. Nothing leaves the room.

Use "qovira serve" to start the server, "qovira migrate" to manage the
database schema, "qovira healthcheck" to probe the local server, and
"qovira version" to print build information.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		newServeCmd(),
		newMigrateCmd(),
		newHealthcheckCmd(),
		newVersionCmd(),
	)

	return root
}
