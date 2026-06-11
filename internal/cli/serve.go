package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/qovira/qovira/internal/config"
	"github.com/qovira/qovira/internal/store"
)

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the Qovira HTTP server",
		Long:  "Start the Qovira HTTP API server (the Docker entrypoint for the application).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, _ := cmd.Flags().GetString("config")

			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}

			s, err := openAndMigrate(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			// HTTP server start is wired in a later issue.
			return errors.New("serve: not yet implemented")
		},
	}

	// Reserve the --config flag so later slices can populate it without
	// breaking the flag surface.
	cmd.Flags().String("config", "", "path to config file (default: auto-discovered)")

	return cmd
}

// openAndMigrate opens the store and, when cfg.AutoMigrate is true, runs all
// pending migrations against the write pool. It is extracted so the boot
// sequence can be exercised in tests without starting an HTTP server.
//
// On success the returned *store.Store is open and the caller is responsible
// for closing it. On error the store is already closed before this function
// returns.
func openAndMigrate(ctx context.Context, cfg *config.Config) (s *store.Store, err error) {
	s, err = store.Open(store.Config{
		Path: store.DBPath(cfg.DataDir),
		Key:  string(cfg.MasterKey),
	})
	if err != nil {
		return nil, fmt.Errorf("serve: open store: %w", err)
	}
	defer func() {
		if err != nil {
			_ = s.Close()
			s = nil
		}
	}()

	if cfg.AutoMigrate {
		runner := store.NewRunner()
		if err = runner.Up(ctx, s.Writer()); err != nil {
			err = fmt.Errorf("serve: auto-migrate: %w", err)
			return
		}
	}

	return s, nil
}
