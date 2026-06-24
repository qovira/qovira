package cli

import (
	"github.com/spf13/cobra"
)

// newAdminCmd constructs the "admin" group command. It prints help when called without a subcommand and owns the
// persistent --config flag so all subcommands inherit it (mirroring the migrate group pattern).
func newAdminCmd() *cobra.Command {
	admin := &cobra.Command{
		Use:   "admin",
		Short: "Administrative operations (password reset, …)",
		Long:  "Administrative operations that require the master key and direct database access.",
		// Print usage when the group is called bare.
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	admin.PersistentFlags().String("config", "", "path to TOML config file (optional; env vars and defaults are used when unset)")

	admin.AddCommand(
		newAdminResetPasswordCmd(),
	)

	return admin
}
