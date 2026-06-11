package cli

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/qovira/qovira/internal/app"
	"github.com/qovira/qovira/internal/config"
	"github.com/qovira/qovira/internal/httpx"
	"github.com/qovira/qovira/internal/logging"
	"github.com/qovira/qovira/internal/store"
)

// denyAllValidator is the temporary TokenValidator injected until the Identity
// & Auth slice lands with a concrete implementation. Every token validation
// attempt returns a non-nil error, causing AuthMiddleware to respond 401 for
// all protected routes. Replace this injection point when the real validator
// is available.
type denyAllValidator struct{}

func (denyAllValidator) ValidateToken(_ context.Context, _ string) (store.Principal, error) {
	return store.Principal{}, errors.New("token validation not yet implemented")
}

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

			logger := logging.NewLogger(os.Stdout, *cfg)

			var validator httpx.TokenValidator = denyAllValidator{}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			a, err := app.New(ctx, cfg, logger, validator, version)
			if err != nil {
				return err
			}

			return a.Run(ctx)
		},
	}

	// Reserve the --config flag so later slices can populate it without
	// breaking the flag surface.
	cmd.Flags().String("config", "", "path to config file (default: auto-discovered)")

	return cmd
}
