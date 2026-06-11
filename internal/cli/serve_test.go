// Tests for the serve boot sequence: openAndMigrate is tested via the
// internal package to verify the factored open+migrate path without starting
// an HTTP server.
package cli

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/qovira/qovira/internal/config"
	"github.com/qovira/qovira/internal/store"
)

// testConfig builds a minimal *config.Config pointed at dir with the given
// autoMigrate value.
func testConfig(t *testing.T, dir string, autoMigrate bool) *config.Config {
	t.Helper()
	return &config.Config{
		MasterKey:   "a-sufficiently-long-passphrase-for-sqlcipher",
		DataDir:     dir,
		HTTPAddr:    ":8080",
		LogLevel:    "info",
		LogFormat:   "json",
		AutoMigrate: autoMigrate,
	}
}

// tableExistsCLI returns true when the named table is visible in sqlite_master.
func tableExistsCLI(t *testing.T, db *sql.DB, name string) bool {
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

// TestOpenAndMigrate_AutoMigrateTrue verifies AC1 (true branch): when
// AutoMigrate=true, openAndMigrate applies all pending migrations so the
// instance table and goose_db_version table exist after it returns.
func TestOpenAndMigrate_AutoMigrateTrue(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := testConfig(t, dir, true)

	s, err := openAndMigrate(context.Background(), cfg)
	if err != nil {
		t.Fatalf("openAndMigrate(autoMigrate=true): %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	}()

	if !tableExistsCLI(t, s.Writer(), "instance") {
		t.Error("instance table not found after openAndMigrate with AutoMigrate=true")
	}
	if !tableExistsCLI(t, s.Writer(), "goose_db_version") {
		t.Error("goose_db_version table not found after openAndMigrate with AutoMigrate=true")
	}
}

// TestOpenAndMigrate_AutoMigrateFalse verifies AC1 (false branch): when
// AutoMigrate=false, openAndMigrate opens the store but does NOT apply
// migrations, so the instance table is absent.
func TestOpenAndMigrate_AutoMigrateFalse(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := testConfig(t, dir, false)

	s, err := openAndMigrate(context.Background(), cfg)
	if err != nil {
		t.Fatalf("openAndMigrate(autoMigrate=false): %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	}()

	if tableExistsCLI(t, s.Writer(), "instance") {
		t.Error("instance table found after openAndMigrate with AutoMigrate=false; expected absent")
	}
}

// TestOpenAndMigrate_ReaderSeesSchema verifies AC4 from the CLI side: after
// openAndMigrate with AutoMigrate=true, the read pool (separate pool, same
// file) observes the migrated schema. This confirms migrations were applied to
// the write pool and the shared WAL propagates the schema to readers.
func TestOpenAndMigrate_ReaderSeesSchema(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := testConfig(t, dir, true)

	s, err := openAndMigrate(context.Background(), cfg)
	if err != nil {
		t.Fatalf("openAndMigrate: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	}()

	if !tableExistsCLI(t, s.Reader(), "instance") {
		t.Error("instance table not visible via read pool after migration")
	}
}

// TestOpenAndMigrate_DBPath verifies that openAndMigrate creates the database
// at the path derived by store.DBPath (not at a hardcoded or arbitrary path).
func TestOpenAndMigrate_DBPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := testConfig(t, dir, false)

	s, err := openAndMigrate(context.Background(), cfg)
	if err != nil {
		t.Fatalf("openAndMigrate: %v", err)
	}
	defer func() { _ = s.Close() }()

	expectedPath := filepath.Clean(store.DBPath(dir))

	// The database file must exist on disk at exactly the expected path.
	if _, err := os.Stat(expectedPath); err != nil {
		t.Errorf("database file not found at expected path %q: %v", expectedPath, err)
	}
}
