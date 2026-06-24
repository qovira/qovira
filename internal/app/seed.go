package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/qovira/qovira/internal/auth"
	"github.com/qovira/qovira/internal/bootstrap"
	"github.com/qovira/qovira/internal/config"
	"github.com/qovira/qovira/internal/store"
)

// seedAdminIfFirstRun runs the first-run admin seeding sequence. It is called by app.New after migrations so the users
// table is guaranteed to exist.
//
// It builds a dedicated *auth.Service using the production defaults (the same params that the auth module uses for
// user-facing operations), checks whether any users exist, and — on a first run with both credentials present —
// creates the initial admin account. On success it logs the admin email at INFO level; the password is never logged
// (config.Secret stays redacted by the type).
//
// The function is a no-op when:
//   - AutoMigrate is false (users table may not exist).
//   - Any user already exists in the database.
//   - Either QOVIRA_ADMIN_EMAIL or QOVIRA_ADMIN_PASSWORD is empty.
func seedAdminIfFirstRun(
	ctx context.Context,
	cfg *config.Config,
	s *store.Store,
	logger *slog.Logger,
) error {
	// Skip seeding when AutoMigrate is false: the users table may not be present, and seeding would fail with
	// "no such table: users". Operators who run migrations out-of-band (qovira migrate) and then start the server
	// without AutoMigrate can still seed by enabling AutoMigrate for the first boot or by using qovira admin
	// reset-password once a user exists. This is an acceptable trade-off documented in the serve command.
	if !cfg.AutoMigrate {
		return nil
	}

	hasher := auth.NewHasher(auth.DefaultParams)
	svc := auth.NewService(s, hasher, auth.DefaultPolicy)

	isFirst, err := bootstrap.IsFirstRun(ctx, s)
	if err != nil {
		return fmt.Errorf("check first run: %w", err)
	}

	seeded, err := bootstrap.MaybeSeedAdmin(
		ctx,
		isFirst,
		cfg.AdminEmail,
		string(cfg.AdminPassword), // extract plaintext from Secret; never logged
		svc,
	)
	if err != nil {
		return fmt.Errorf("seed admin: %w", err)
	}
	if seeded {
		logger.Info("app: initial admin account created", "email", cfg.AdminEmail)
	}
	return nil
}
