package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/qovira/qovira/internal/config"
	"github.com/qovira/qovira/internal/store"
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

	// --config is a persistent flag on the parent so all subcommands inherit it.
	migrate.PersistentFlags().String("config", "", "path to config file (default: auto-discovered)")

	migrate.AddCommand(
		newMigrateUpCmd(),
		newMigrateStatusCmd(),
		newMigrateDownCmd(),
	)

	return migrate
}

// migrateConfig loads configuration from the --config flag on cmd (or its
// parent, since the flag is persistent) and opens the store. The caller is
// responsible for closing the returned *store.Store.
func migrateConfig(cmd *cobra.Command) (*config.Config, *store.Store, error) {
	cfgPath, _ := cmd.Flags().GetString("config")

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, fmt.Errorf("migrate: load config: %w", err)
	}

	s, err := store.Open(store.Config{
		Path: store.DBPath(cfg.DataDir),
		Key:  string(cfg.MasterKey),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("migrate: open store: %w", err)
	}

	return cfg, s, nil
}

func newMigrateUpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Apply all pending migrations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, s, err := migrateConfig(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			runner := store.NewRunner()
			if err := runner.Up(cmd.Context(), s.Writer()); err != nil {
				return fmt.Errorf("migrate up: %w", err)
			}
			return nil
		},
	}
}

func newMigrateStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Print the current migration status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, s, err := migrateConfig(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			runner := store.NewRunner()
			if err := runner.Status(cmd.Context(), s.Writer(), cmd.OutOrStdout()); err != nil {
				return fmt.Errorf("migrate status: %w", err)
			}
			return nil
		},
	}
}

func newMigrateDownCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Roll back the last applied migration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, s, err := migrateConfig(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			runner := store.NewRunner()
			if err := runner.Down(cmd.Context(), s.Writer()); err != nil {
				return fmt.Errorf("migrate down: %w", err)
			}
			return nil
		},
	}
}
