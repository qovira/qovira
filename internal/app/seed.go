package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/qovira/qovira/internal/auth"
	"github.com/qovira/qovira/internal/bootstrap"
	"github.com/qovira/qovira/internal/config"
	"github.com/qovira/qovira/internal/gateway"
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

// seedGatewayConfig writes the model gateway's primary endpoint coordinates into the settings store from the boot
// environment, when they are set. It is called by app.New after migrations so the settings table exists.
//
// Unlike admin seeding, this is NOT gated on first run: the values are upserted on every boot whenever the
// QOVIRA_GATEWAY_* variables are present, because the environment is the only configuration surface for the gateway in
// v0.1 — an operator changes the endpoint by changing the environment and restarting. When the variables are unset the
// stored settings are left untouched, so a future in-app settings surface can own them.
//
// config validation guarantees the three values are set together or not at all, so a non-empty base URL implies all
// three are present. The API key is written from the redacted config.Secret via an explicit string conversion; it is
// never logged (only the non-secret base URL and model are).
//
// Seeding is skipped when AutoMigrate is false, mirroring admin seeding: the settings table may not exist, and a write
// would fail with "no such table: settings". Operators who migrate out-of-band can still configure the gateway by
// enabling AutoMigrate for one boot.
func seedGatewayConfig(
	ctx context.Context,
	cfg *config.Config,
	s *store.Store,
	logger *slog.Logger,
) error {
	if !cfg.AutoMigrate || cfg.GatewayBaseURL == "" {
		return nil
	}

	ns := s.Settings().Namespace(gateway.NamespaceModelGateway)
	pairs := []struct{ key, value string }{
		{gateway.KeyPrimaryBaseURL, cfg.GatewayBaseURL},
		{gateway.KeyPrimaryAPIKey, string(cfg.GatewayAPIKey)}, // extract plaintext from Secret; never logged
		{gateway.KeyPrimaryModel, cfg.GatewayModel},
	}
	for _, p := range pairs {
		if err := ns.Set(ctx, p.key, p.value); err != nil {
			return fmt.Errorf("seed gateway setting %s: %w", p.key, err)
		}
	}
	logger.Info("app: model gateway configuration seeded from environment",
		"baseURL", cfg.GatewayBaseURL, "model", cfg.GatewayModel)
	return nil
}
