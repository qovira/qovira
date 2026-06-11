package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

func newMigrateCmd() *cobra.Command {
	migrate := &cobra.Command{
		Use:   "migrate",
		Short: "Manage database schema migrations",
		Long:  "Run or inspect goose migrations against the Qovira SQLCipher database.",
		// Require a subcommand; print usage when called bare.
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	migrate.AddCommand(
		newMigrateUpCmd(),
		newMigrateStatusCmd(),
		newMigrateDownCmd(),
	)

	return migrate
}

func newMigrateUpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Apply all pending migrations",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// Stub: wired to internal/store in QOV-40.
			return errors.New("migrate up: not yet implemented")
		},
	}
}

func newMigrateStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Print the current migration status",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// Stub: wired to internal/store in QOV-40.
			return errors.New("migrate status: not yet implemented")
		},
	}
}

func newMigrateDownCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Roll back the last applied migration",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// Stub: wired to internal/store in QOV-40.
			return errors.New("migrate down: not yet implemented")
		},
	}
}
