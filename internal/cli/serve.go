package cli

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/qovira/qovira/internal/app"
	"github.com/qovira/qovira/internal/auth"
	"github.com/qovira/qovira/internal/config"
	"github.com/qovira/qovira/internal/harness"
	"github.com/qovira/qovira/internal/httpx"
	"github.com/qovira/qovira/internal/logging"
	"github.com/qovira/qovira/internal/store"
)

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the Qovira server",
		Long:  "Start the Qovira server — the JSON API, the realtime event stream, and the bundled web UI. This is the container entrypoint for the application.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, _ := cmd.Flags().GetString("config")

			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}

			logger := logging.NewLogger(os.Stdout, *cfg)

			// newValidator builds the real token validator once the store is open. Session config is wired via
			// DefaultSessionConfig (per-instance tuning lives in the DB config layer, which is a later slice).
			newValidator := func(s *store.Store) httpx.TokenValidator {
				sessions := auth.NewSessions(s, auth.DefaultSessionConfig)
				return auth.NewAuthenticator(sessions)
			}

			// authCtor builds the auth HTTP module from the store. DefaultParams, DefaultPolicy, and
			// DefaultSessionConfig are the production values; per-instance overrides live in the DB settings layer (a
			// later slice). logger is forwarded so internal errors are diagnosable server-side.
			authCtor := app.AuthModuleCtor(auth.DefaultParams, auth.DefaultPolicy, auth.DefaultSessionConfig, logger)

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			a, err := app.New(ctx, cfg, logger, newValidator, version, harness.Config{}, authCtor)
			if err != nil {
				return err
			}

			return a.Run(ctx)
		},
	}

	// Reserve the --config flag so later slices can populate it without breaking the flag surface.
	cmd.Flags().String("config", "", "path to TOML config file (optional; env vars and defaults are used when unset)")

	return cmd
}
