package app_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/qovira/qovira/internal/app"
	"github.com/qovira/qovira/internal/auth"
	"github.com/qovira/qovira/internal/config"
	"github.com/qovira/qovira/internal/gateway"
	"github.com/qovira/qovira/internal/harness"
	"github.com/qovira/qovira/internal/store"
)

// seedTestKey is the encryption key used across admin-seeding tests.
const seedTestKey = "a-sufficiently-long-passphrase-for-sqlcipher"

// fastSeedParams are low-cost argon2id parameters for tests — real hashing, minimal wall-clock time.
var fastSeedParams = auth.Params{
	Memory:  64,
	Time:    1,
	Threads: 1,
	KeyLen:  32,
	SaltLen: 16,
}

// openTestStore opens a migrated *store.Store for seed tests that need direct store access after app.New runs.
func openTestStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	s, err := store.Open(store.Config{
		Path:         filepath.Join(dir, "qovira.db"),
		Key:          seedTestKey,
		ReadPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("store.Open (direct): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newSeedCfg builds a Config with admin credentials set, pointing at dir.
func newSeedCfg(t *testing.T, dir, adminEmail, adminPassword string) *config.Config {
	t.Helper()
	return &config.Config{
		MasterKey:     config.Secret(seedTestKey),
		DataDir:       dir,
		HTTPAddr:      "127.0.0.1:0",
		LogLevel:      "error",
		LogFormat:     "json",
		AutoMigrate:   true,
		AdminEmail:    adminEmail,
		AdminPassword: config.Secret(adminPassword),
	}
}

// seedAuthCtor builds an auth module constructor that uses fastSeedParams so tests run quickly while still exercising
// the real argon2id code path.
func seedAuthCtor() func(*store.Store) app.Module {
	return app.AuthModuleCtor(fastSeedParams, auth.DefaultPolicy, auth.DefaultSessionConfig, discardLogger())
}

// TestNew_SeedsAdminOnFirstRun verifies that when both QOVIRA_ADMIN_EMAIL and QOVIRA_ADMIN_PASSWORD are set and no
// users exist, app.New creates exactly one admin user whose password authenticates via auth.Service.Authenticate.
func TestNew_SeedsAdminOnFirstRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := newSeedCfg(t, dir, "admin@example.com", "correct-horse-battery")

	a, err := app.New(context.Background(), cfg, discardLogger(), denyAllCtor, "test", harness.Config{}, seedAuthCtor())
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	cleanupApp(t, a)

	// Open the same DB directly (app.New has already closed nothing — the App is still running) and verify the admin
	// user row exists with role "admin".
	s := openTestStore(t, dir)
	runner := store.NewRunner()
	// Migrations were applied by app.New; just need a store reference here.
	_ = runner

	hasher := auth.NewHasher(fastSeedParams)
	svc := auth.NewService(s, hasher, auth.DefaultPolicy)

	user, err := svc.GetUserByEmail(context.Background(), "admin@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if user.Role != auth.RoleAdmin {
		t.Errorf("Role = %q, want %q", user.Role, auth.RoleAdmin)
	}

	// The password must authenticate correctly.
	authed, err := svc.Authenticate(context.Background(), "admin@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if authed.ID != user.ID {
		t.Errorf("Authenticate returned wrong user ID: got %q, want %q", authed.ID, user.ID)
	}
}

// TestNew_NoSeedWhenUsersExist verifies that when a user already exists, app.New does not create a second user even
// when admin credentials are set.
func TestNew_NoSeedWhenUsersExist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// First boot: seed the admin.
	cfg1 := newSeedCfg(t, dir, "admin@example.com", "correct-horse-battery")
	a1, err := app.New(context.Background(), cfg1, discardLogger(), denyAllCtor, "test", harness.Config{}, seedAuthCtor())
	if err != nil {
		t.Fatalf("first app.New: %v", err)
	}
	// Shut down the first app cleanly before reopening.
	ctx1, cancel1 := context.WithCancel(context.Background())
	cancel1()
	_ = a1.Run(ctx1)

	// Second boot: credentials still set — must NOT create a second user.
	cfg2 := newSeedCfg(t, dir, "second@example.com", "another-password-here")
	a2, err := app.New(context.Background(), cfg2, discardLogger(), denyAllCtor, "test", harness.Config{}, seedAuthCtor())
	if err != nil {
		t.Fatalf("second app.New: %v", err)
	}
	cleanupApp(t, a2)

	// Verify: only one user in the DB (the original admin).
	s := openTestStore(t, dir)
	hasher := auth.NewHasher(fastSeedParams)
	svc := auth.NewService(s, hasher, auth.DefaultPolicy)

	_, err = svc.GetUserByEmail(context.Background(), "second@example.com")
	if !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("second user exists after second boot; expected not found, got: %v", err)
	}
}

// TestNew_NoSeedWhenCredsEmpty verifies that when either or both admin env vars are empty, app.New does not create any
// user (no error, no user).
func TestNew_NoSeedWhenCredsEmpty(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		adminEmail    string
		adminPassword string
	}{
		{"both empty", "", ""},
		{"email only", "admin@example.com", ""},
		{"password only", "", "correct-horse-battery"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			cfg := &config.Config{
				MasterKey:     config.Secret(seedTestKey),
				DataDir:       dir,
				HTTPAddr:      "127.0.0.1:0",
				LogLevel:      "error",
				LogFormat:     "json",
				AutoMigrate:   true,
				AdminEmail:    tc.adminEmail,
				AdminPassword: config.Secret(tc.adminPassword),
			}

			a, err := app.New(context.Background(), cfg, discardLogger(), denyAllCtor, "test", harness.Config{}, seedAuthCtor())
			if err != nil {
				t.Fatalf("app.New: %v", err)
			}
			cleanupApp(t, a)

			// No user should exist.
			s := openTestStore(t, dir)
			hasher := auth.NewHasher(fastSeedParams)
			svc := auth.NewService(s, hasher, auth.DefaultPolicy)

			if tc.adminEmail != "" {
				_, err := svc.GetUserByEmail(context.Background(), tc.adminEmail)
				if !errors.Is(err, auth.ErrUserNotFound) {
					t.Errorf("user unexpectedly created for case %q; GetUserByEmail = %v", tc.name, err)
				}
			} else {
				// Verify by checking HasAnyUser on the store.
				has, err := s.HasAnyUser(context.Background())
				if err != nil {
					t.Fatalf("HasAnyUser: %v", err)
				}
				if has {
					t.Errorf("HasAnyUser() = true with empty creds; want false")
				}
			}
		})
	}
}

// newGatewaySeedCfg builds a Config with the model gateway primary endpoint set (no admin credentials), pointing at dir.
func newGatewaySeedCfg(t *testing.T, dir, baseURL, apiKey, model string) *config.Config {
	t.Helper()
	return &config.Config{
		MasterKey:      config.Secret(seedTestKey),
		DataDir:        dir,
		HTTPAddr:       "127.0.0.1:0",
		LogLevel:       "error",
		LogFormat:      "json",
		AutoMigrate:    true,
		GatewayBaseURL: baseURL,
		GatewayAPIKey:  config.Secret(apiKey),
		GatewayModel:   model,
	}
}

// assertGatewaySetting reads a model_gateway setting back from a freshly-opened store and asserts its value.
func assertGatewaySetting(t *testing.T, dir, key, want string) {
	t.Helper()
	s := openTestStore(t, dir)
	got, found, err := s.Settings().Namespace(gateway.NamespaceModelGateway).Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get %q: %v", key, err)
	}
	if !found || got != want {
		t.Errorf("setting %q = %q (found=%v), want %q", key, got, found, want)
	}
}

// TestNew_SeedsGatewayConfigFromEnv verifies that when the gateway config fields are set, app.New writes the primary
// endpoint coordinates into the model_gateway settings namespace — the same keys the gateway reads.
func TestNew_SeedsGatewayConfigFromEnv(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := newGatewaySeedCfg(t, dir, "https://api.example.com/v1", "sk-secret-xyz", "qwen2.5")

	a, err := app.New(context.Background(), cfg, discardLogger(), denyAllCtor, "test", harness.Config{}, seedAuthCtor())
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	cleanupApp(t, a)

	assertGatewaySetting(t, dir, gateway.KeyPrimaryBaseURL, "https://api.example.com/v1")
	assertGatewaySetting(t, dir, gateway.KeyPrimaryAPIKey, "sk-secret-xyz")
	assertGatewaySetting(t, dir, gateway.KeyPrimaryModel, "qwen2.5")
}

// TestNew_NoGatewaySeedWhenUnset verifies that when no gateway config is provided, app.New leaves the model_gateway
// namespace empty.
func TestNew_NoGatewaySeedWhenUnset(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := newSeedCfg(t, dir, "", "") // no admin, no gateway

	a, err := app.New(context.Background(), cfg, discardLogger(), denyAllCtor, "test", harness.Config{}, seedAuthCtor())
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	cleanupApp(t, a)

	s := openTestStore(t, dir)
	_, found, err := s.Settings().Namespace(gateway.NamespaceModelGateway).Get(context.Background(), gateway.KeyPrimaryBaseURL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Error("expected no gateway baseURL setting when unset, but one was written")
	}
}

// TestNew_GatewaySeedOverwritesExisting verifies the upsert-every-boot semantic: a boot with gateway config overwrites
// stale values left by a prior boot, because the environment is the source of truth in v0.1.
func TestNew_GatewaySeedOverwritesExisting(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Simulate a prior boot: migrate, then write stale gateway settings directly, then close the handle.
	pre := openTestStore(t, dir)
	if err := store.NewRunner().Up(context.Background(), pre.Writer()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	preNS := pre.Settings().Namespace(gateway.NamespaceModelGateway)
	stale := map[string]string{
		gateway.KeyPrimaryBaseURL: "https://stale.example.com/v1",
		gateway.KeyPrimaryAPIKey:  "sk-stale",
		gateway.KeyPrimaryModel:   "model-stale",
	}
	for k, v := range stale {
		if err := preNS.Set(context.Background(), k, v); err != nil {
			t.Fatalf("pre-seed %q: %v", k, err)
		}
	}
	_ = pre.Close()

	// Boot with fresh config — it must overwrite every stale value.
	cfg := newGatewaySeedCfg(t, dir, "https://fresh.example.com/v1", "sk-fresh", "model-fresh")
	a, err := app.New(context.Background(), cfg, discardLogger(), denyAllCtor, "test", harness.Config{}, seedAuthCtor())
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	cleanupApp(t, a)

	assertGatewaySetting(t, dir, gateway.KeyPrimaryBaseURL, "https://fresh.example.com/v1")
	assertGatewaySetting(t, dir, gateway.KeyPrimaryAPIKey, "sk-fresh")
	assertGatewaySetting(t, dir, gateway.KeyPrimaryModel, "model-fresh")
}
