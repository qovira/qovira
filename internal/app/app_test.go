package app_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/app"
	"github.com/qovira/qovira/internal/capability"
	"github.com/qovira/qovira/internal/config"
	"github.com/qovira/qovira/internal/events"
	"github.com/qovira/qovira/internal/harness"
	"github.com/qovira/qovira/internal/httpx"
	"github.com/qovira/qovira/internal/store"
)

// discardLogger returns a slog.Logger that discards all output below Error.
// Used in tests that do not need to inspect log lines.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// testConfig builds a minimal *config.Config pointing at dir.
func testConfig(t *testing.T, dir string, autoMigrate bool) *config.Config {
	t.Helper()
	return &config.Config{
		MasterKey:   "a-sufficiently-long-passphrase-for-sqlcipher",
		DataDir:     dir,
		HTTPAddr:    "127.0.0.1:0",
		LogLevel:    "error",
		LogFormat:   "json",
		AutoMigrate: autoMigrate,
	}
}

// denyAllValidator satisfies httpx.TokenValidator and rejects every token.
type denyAllValidator struct{}

func (denyAllValidator) ValidateToken(_ context.Context, _ string) (store.Principal, error) {
	return store.Principal{}, errors.New("token validation not yet implemented")
}

// denyAllCtor is a newValidator constructor that always returns a denyAllValidator.
// Use it in tests that exercise app.New without needing real token resolution.
func denyAllCtor(_ *store.Store) httpx.TokenValidator { return denyAllValidator{} }

// cleanupApp shuts down a in its cleanup hook without leaking goroutines.
func cleanupApp(t *testing.T, a *app.App) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = a.Server().Shutdown(ctx)
		_ = a.Store().Close()
	})
}

// TestNew_FailFast_EmptyKey verifies that app.New returns a non-nil error and
// leaks no open store when the master key is empty (store.Open will fail).
func TestNew_FailFast_EmptyKey(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		MasterKey:   "", // intentionally empty
		DataDir:     t.TempDir(),
		HTTPAddr:    "127.0.0.1:0",
		LogLevel:    "error",
		LogFormat:   "json",
		AutoMigrate: false,
	}

	a, err := app.New(context.Background(), cfg, discardLogger(), denyAllCtor, "test", harness.Config{})
	if err == nil {
		t.Fatal("app.New returned nil error for empty MasterKey; want non-nil")
	}
	if a != nil {
		t.Error("app.New returned non-nil *App alongside an error; expected nil")
	}
}

// TestNew_Success_AutoMigrateTrue verifies that app.New succeeds with a valid
// config and AutoMigrate=true, returning a non-nil *App with a wired server.
func TestNew_Success_AutoMigrateTrue(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := testConfig(t, dir, true)

	a, err := app.New(context.Background(), cfg, discardLogger(), denyAllCtor, "test", harness.Config{})
	if err != nil {
		t.Fatalf("app.New: unexpected error: %v", err)
	}
	cleanupApp(t, a)

	if a.Server() == nil {
		t.Error("app.Server() is nil after successful New")
	}
}

// TestNew_Healthz_Returns200AndVersion verifies that after a successful New,
// a GET /healthz returns 200 and that the version string passed to New is
// threaded through to the response body. This proves the single-source-of-truth
// wiring: no separate package-level var, just the parameter.
func TestNew_Healthz_Returns200AndVersion(t *testing.T) {
	t.Parallel()

	const wantVersion = "test"

	dir := t.TempDir()
	cfg := testConfig(t, dir, true)

	a, err := app.New(context.Background(), cfg, discardLogger(), denyAllCtor, wantVersion, harness.Config{})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	cleanupApp(t, a)

	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	a.Server().Handler.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Errorf("GET /healthz: status = %d, want 200", rr.Code)
	}

	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET /healthz: unmarshal body: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("GET /healthz: body.status = %q, want \"ok\"", body.Status)
	}
	if body.Version != wantVersion {
		t.Errorf("GET /healthz: body.version = %q, want %q", body.Version, wantVersion)
	}
}

// TestNew_ProtectedRoute_NoToken_Returns401 verifies that a protected API
// route with no Authorization header returns 401 (auth middleware live with
// deny-all validator).
func TestNew_ProtectedRoute_NoToken_Returns401(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := testConfig(t, dir, true)

	a, err := app.New(context.Background(), cfg, discardLogger(), denyAllCtor, "test", harness.Config{})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	cleanupApp(t, a)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/anything", nil)
	rr := httptest.NewRecorder()
	a.Server().Handler.ServeHTTP(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/anything (no token): status = %d, want 401", rr.Code)
	}

	ct := rr.Header().Get("Content-Type")
	if ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

// TestNew_ProtectedRoute_WithBearerToken_Returns401 verifies that a Bearer
// token is rejected by the deny-all validator, yielding 401.
func TestNew_ProtectedRoute_WithBearerToken_Returns401(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := testConfig(t, dir, false)

	a, err := app.New(context.Background(), cfg, discardLogger(), denyAllCtor, "test", harness.Config{})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	cleanupApp(t, a)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/anything", nil)
	r.Header.Set("Authorization", "Bearer sometoken")
	rr := httptest.NewRecorder()
	a.Server().Handler.ServeHTTP(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/anything (Bearer token): status = %d, want 401", rr.Code)
	}
}

// shutdownTimeout mirrors the constant in package app so the test's wait
// margin is meaningful.
const shutdownTimeout = 15 * time.Second

// waitReady polls the TCP address addr until a connection succeeds or ctx is
// cancelled. It returns true when the server is accepting connections, false
// when ctx expires first.
func waitReady(ctx context.Context, addr string) bool {
	for {
		conn, err := net.DialTimeout("tcp", addr, 10*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		select {
		case <-ctx.Done():
			return false
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// TestRun_GracefulShutdown verifies that cancelling ctx makes Run return
// within the bounded shutdown timeout and the store is closed afterward.
func TestRun_GracefulShutdown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := testConfig(t, dir, false)
	cfg.HTTPAddr = "127.0.0.1:0"

	a, err := app.New(context.Background(), cfg, discardLogger(), denyAllCtor, "test", harness.Config{})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan error, 1)
	go func() {
		runDone <- a.Run(ctx)
	}()

	// Wait for the server to bind its ephemeral port rather than sleeping a
	// fixed duration. ListenAddr blocks until BaseContext fires (i.e. the
	// socket is open), then we dial once to confirm reachability.
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer readyCancel()

	boundAddr := a.ListenAddr(readyCtx)
	if boundAddr == "" {
		cancel()
		t.Fatal("server did not become ready within 5 s")
	}
	if !waitReady(readyCtx, boundAddr) {
		cancel()
		t.Fatal("server bound but did not accept connections within 5 s")
	}

	cancel()

	// Run must return within the shutdown timeout plus a small margin.
	select {
	case err := <-runDone:
		if err != nil {
			t.Errorf("app.Run returned unexpected error: %v", err)
		}
	case <-time.After(shutdownTimeout + 2*time.Second):
		t.Fatal("app.Run did not return within expected shutdown window")
	}

	// After shutdown, the store should be closed. Attempting a query against the
	// write pool should fail with "sql: database is closed".
	var n int
	queryErr := a.Store().Writer().QueryRow("SELECT 1").Scan(&n)
	if queryErr == nil {
		t.Error("query on store.Writer() after shutdown returned nil error; expected store to be closed")
	}
}

// fakeModule is a test double for app.Module that mounts a route and returns
// one tool.
type fakeModule struct{}

func (fakeModule) Name() string { return "fake" }

func (fakeModule) Routes(r *httpx.Router) {
	r.HandleFunc("GET /api/v1/fake", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"fake":true}`))
	})
}

func (fakeModule) Tools() []capability.Tool {
	return []capability.Tool{
		{Name: "fake.ping", Description: "A fake tool for testing."},
	}
}

// fakeModuleCtor is a module-constructor form of fakeModule for use with the
// updated app.New signature that accepts func(*store.Store) app.Module.
func fakeModuleCtor(_ *store.Store) app.Module { return fakeModule{} }

// TestNew_ModuleSeam_RouteAndAuth verifies that a Module passed to New mounts
// its route on the shared mux and that auth is live on that route (deny-all
// validator → 401 with no token, instead of 404 from the catch-all).
func TestNew_ModuleSeam_RouteAndAuth(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := testConfig(t, dir, false)

	a, err := app.New(context.Background(), cfg, discardLogger(), denyAllCtor, "test", harness.Config{}, fakeModuleCtor)
	if err != nil {
		t.Fatalf("app.New with fakeModule: %v", err)
	}
	cleanupApp(t, a)

	// No auth token → 401 (auth middleware intercepts before the handler).
	// This proves the route is mounted (both catch-all and the module route
	// go through auth middleware, so 401 vs 404 does not distinguish them here —
	// but combined with TestNew_ModuleSeam_DirectRouterMount below it covers
	// the seam completely).
	r := httptest.NewRequest(http.MethodGet, "/api/v1/fake", nil)
	rr := httptest.NewRecorder()
	a.Server().Handler.ServeHTTP(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/fake (no token) status = %d, want 401", rr.Code)
	}
}

// TestNew_ModuleSeam_DirectRouterMount confirms the Module.Routes seam by
// mounting a fakeModule onto a router passed to NewServer (no auth middleware)
// and verifying the route returns 200. NewServer registers the catch-all
// /api/v1/{path...} after the module's specific "GET /api/v1/fake" pattern, so
// the specific pattern takes precedence and the module handler is invoked.
func TestNew_ModuleSeam_DirectRouterMount(t *testing.T) {
	t.Parallel()

	router := httpx.NewRouter()
	fakeModule{}.Routes(router)

	// Construct a bare server with no middleware so we hit the handler directly.
	// NewServer adds the catch-all /api/v1/{path...} after module routes are
	// mounted, so fakeModule's "GET /api/v1/fake" wins by specificity.
	srv := httpx.NewServer("127.0.0.1:0", "test", router, events.NewBus())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fake", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /api/v1/fake (direct mount, no middleware): status = %d, want 200", rec.Code)
	}
}

// tableExists returns true when the named table is visible in sqlite_master.
func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	err := db.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name,
	).Scan(&n)
	if err != nil {
		t.Fatalf("sqlite_master query for %q: %v", name, err)
	}
	return n > 0
}

// TestNew_AutoMigrate_True verifies that when AutoMigrate=true, app.New applies
// all pending migrations so the instance table and goose_db_version table exist
// on the write pool after construction.
func TestNew_AutoMigrate_True(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := testConfig(t, dir, true)

	a, err := app.New(context.Background(), cfg, discardLogger(), denyAllCtor, "test", harness.Config{})
	if err != nil {
		t.Fatalf("app.New(AutoMigrate=true): %v", err)
	}
	cleanupApp(t, a)

	if !tableExists(t, a.Store().Writer(), "instance") {
		t.Error("instance table not found after app.New with AutoMigrate=true")
	}
	if !tableExists(t, a.Store().Writer(), "goose_db_version") {
		t.Error("goose_db_version table not found after app.New with AutoMigrate=true")
	}
}

// TestNew_RegistersHarnessSweepPeriodic verifies that app.New wires the harness
// sweep as a system-scoped periodic job: exactly one jobs row keyed
// "harness.sweep_confirmations" exists, with a NULL user_id (system scope) and a
// recurrence (interval) set. This is the wiring seam for the scheduler's periodic
// machinery; the sweep handler's behavior itself lives in the harness.
func TestNew_RegistersHarnessSweepPeriodic(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := testConfig(t, dir, true)

	a, err := app.New(context.Background(), cfg, discardLogger(), denyAllCtor, "test", harness.Config{})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	cleanupApp(t, a)

	var (
		count        int
		userID       sql.NullString
		intervalSecs sql.NullInt64
	)
	row := a.Store().Writer().QueryRowContext(context.Background(),
		`SELECT count(*),
		        max(user_id),
		        max(interval_secs)
		 FROM jobs WHERE key = 'harness.sweep_confirmations'`)
	if err := row.Scan(&count, &userID, &intervalSecs); err != nil {
		t.Fatalf("query harness sweep job: %v", err)
	}
	if count != 1 {
		t.Errorf("harness.sweep_confirmations job count = %d, want exactly 1", count)
	}
	if userID.Valid {
		t.Errorf("harness sweep user_id = %q, want NULL (system scope)", userID.String)
	}
	if !intervalSecs.Valid || intervalSecs.Int64 != 60 {
		t.Errorf("harness sweep interval_secs = %v, want 60 (every minute)", intervalSecs)
	}
}

// TestNew_AutoMigrate_False verifies that when AutoMigrate=false, app.New opens
// the store but does NOT apply migrations, so the instance table is absent.
func TestNew_AutoMigrate_False(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := testConfig(t, dir, false)

	a, err := app.New(context.Background(), cfg, discardLogger(), denyAllCtor, "test", harness.Config{})
	if err != nil {
		t.Fatalf("app.New(AutoMigrate=false): %v", err)
	}
	cleanupApp(t, a)

	if tableExists(t, a.Store().Writer(), "instance") {
		t.Error("instance table found after app.New with AutoMigrate=false; expected absent")
	}
}

// TestNew_AutoMigrate_ReaderSeesSchema verifies that after app.New with
// AutoMigrate=true, the read pool (separate pool, same file) observes the
// migrated schema. Confirms migrations were applied to the write pool and the
// shared WAL propagates the schema to readers.
func TestNew_AutoMigrate_ReaderSeesSchema(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := testConfig(t, dir, true)

	a, err := app.New(context.Background(), cfg, discardLogger(), denyAllCtor, "test", harness.Config{})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	cleanupApp(t, a)

	if !tableExists(t, a.Store().Reader(), "instance") {
		t.Error("instance table not visible via read pool after migration")
	}
}

// TestNew_DBPath verifies that app.New creates the database at the path derived
// by store.DBPath (not at a hardcoded or arbitrary path).
func TestNew_DBPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := testConfig(t, dir, false)

	a, err := app.New(context.Background(), cfg, discardLogger(), denyAllCtor, "test", harness.Config{})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	cleanupApp(t, a)

	expectedPath := filepath.Clean(store.DBPath(dir))
	if _, err := os.Stat(expectedPath); err != nil {
		t.Errorf("database file not found at expected path %q: %v", expectedPath, err)
	}
}

// TestDenyAllValidator_ReturnsError verifies the deny-all placeholder returns
// a non-nil error for any token.
func TestDenyAllValidator_ReturnsError(t *testing.T) {
	t.Parallel()

	var v httpx.TokenValidator = denyAllValidator{}
	_, err := v.ValidateToken(context.Background(), "anytoken")
	if err == nil {
		t.Error("denyAllValidator.ValidateToken returned nil error; want non-nil")
	}
}
