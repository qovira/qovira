package cli

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/qovira/qovira/internal/store"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// migrateTestKey is the SQLCipher master key for all migrate-command tests. Must be at least 16 bytes to pass config
// validation.
const migrateTestKey = "migrate-test-key-which-is-long-enough"

// migrateEnv returns the env map required for the CLI to open the store at dataDir.
func migrateEnv(dataDir string) map[string]string {
	return map[string]string{
		"QOVIRA_MASTER_KEY": migrateTestKey,
		"QOVIRA_DATA_DIR":   dataDir,
	}
}

// runMigrateCmd sets env vars via t.Setenv, then delegates to runCmd. Callers must not mark the parent test
// t.Parallel() because t.Setenv is incompatible with parallel execution.
func runMigrateCmd(t *testing.T, env map[string]string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	return runCmd(t, args...)
}

// openMigrateStore opens a bare (un-migrated) SQLCipher store in dataDir. The store is registered for cleanup via
// t.Cleanup.
func openMigrateStore(t *testing.T, dataDir string) *store.Store {
	t.Helper()
	s, err := store.Open(store.Config{
		Path:         store.DBPath(dataDir),
		Key:          migrateTestKey,
		ReadPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if cerr := s.Close(); cerr != nil {
			t.Errorf("store.Close: %v", cerr)
		}
	})
	return s
}

// tableExists reports whether the named table is present in sqlite_master.
func tableExistsMigrate(t *testing.T, db *sql.DB, name string) bool {
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

// ── AC: migrate up applies migrations ────────────────────────────────────────

// TestMigrateUp_Success verifies that "migrate up" against a fresh store applies all pending migrations. It asserts
// that the goose_db_version tracking table and the instance table (an application migration) are both present after
// the command returns.
func TestMigrateUp_Success(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel().
	dataDir := t.TempDir()

	_, stderr, err := runMigrateCmd(t, migrateEnv(dataDir), "migrate", "up")
	if err != nil {
		t.Fatalf("migrate up: %v\nstderr: %s", err, stderr)
	}

	// Open the store directly to verify the schema was applied.
	s := openMigrateStore(t, dataDir)

	if !tableExistsMigrate(t, s.Writer(), "goose_db_version") {
		t.Error("goose_db_version table not found after migrate up")
	}
	if !tableExistsMigrate(t, s.Writer(), "instance") {
		t.Error("instance table not found after migrate up — migrations were not applied")
	}
}

// TestMigrateUp_Idempotent verifies that running "migrate up" twice on the same store does not error (goose is
// idempotent on an already-current schema).
func TestMigrateUp_Idempotent(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel().
	dataDir := t.TempDir()

	for i := range 2 {
		_, stderr, err := runMigrateCmd(t, migrateEnv(dataDir), "migrate", "up")
		if err != nil {
			t.Fatalf("migrate up (run %d): %v\nstderr: %s", i+1, err, stderr)
		}
	}
}

// ── AC: migrate status reflects applied migrations ────────────────────────────

// TestMigrateStatus_AfterUp verifies that "migrate status" prints applied migration entries to stdout after
// "migrate up" has been run. The status output must contain the word "applied" (goose State string) at least once.
func TestMigrateStatus_AfterUp(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel().
	dataDir := t.TempDir()

	// First apply migrations so there is something to report.
	if _, stderr, err := runMigrateCmd(t, migrateEnv(dataDir), "migrate", "up"); err != nil {
		t.Fatalf("migrate up (pre-condition): %v\nstderr: %s", err, stderr)
	}

	stdout, stderr, err := runMigrateCmd(t, migrateEnv(dataDir), "migrate", "status")
	if err != nil {
		t.Fatalf("migrate status: %v\nstderr: %s", err, stderr)
	}

	if !strings.Contains(stdout, "applied") {
		t.Errorf("migrate status output missing %q\ngot:\n%s", "applied", stdout)
	}
}

// TestMigrateStatus_BeforeUp verifies that "migrate status" on a fresh database reports pending (not-yet-applied)
// migrations. The output must contain the word "pending".
func TestMigrateStatus_BeforeUp(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel().
	dataDir := t.TempDir()

	// Open the store so the SQLCipher file is created, but do NOT run Up.
	s := openMigrateStore(t, dataDir)
	// Close it before the CLI opens its own handle to the same file.
	if err := s.Close(); err != nil {
		t.Fatalf("pre-close store: %v", err)
	}

	stdout, stderr, err := runMigrateCmd(t, migrateEnv(dataDir), "migrate", "status")
	if err != nil {
		t.Fatalf("migrate status (before up): %v\nstderr: %s", err, stderr)
	}

	if !strings.Contains(stdout, "pending") {
		t.Errorf("migrate status output missing %q before any up\ngot:\n%s", "pending", stdout)
	}
}

// ── AC: migrate down rolls back the last migration ────────────────────────────

// TestMigrateDown_AfterUp verifies that "migrate down" succeeds after a full "migrate up" and rolls back exactly one
// migration. We verify by checking that at least one migration is now pending in "migrate status" output after the
// rollback.
func TestMigrateDown_AfterUp(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel().
	dataDir := t.TempDir()

	if _, stderr, err := runMigrateCmd(t, migrateEnv(dataDir), "migrate", "up"); err != nil {
		t.Fatalf("migrate up (pre-condition): %v\nstderr: %s", err, stderr)
	}

	_, stderr, err := runMigrateCmd(t, migrateEnv(dataDir), "migrate", "down")
	if err != nil {
		t.Fatalf("migrate down: %v\nstderr: %s", err, stderr)
	}

	// After rolling back one migration, status must report at least one pending.
	stdout, stderr, err := runMigrateCmd(t, migrateEnv(dataDir), "migrate", "status")
	if err != nil {
		t.Fatalf("migrate status (after down): %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "pending") {
		t.Errorf("migrate status after down: expected at least one pending migration\ngot:\n%s", stdout)
	}
}

// ── AC: no master key → fail fast ────────────────────────────────────────────

// TestMigrateUp_NoMasterKey verifies that "migrate up" fails fast when QOVIRA_MASTER_KEY is absent (mirrors
// TestCommandWiring; included here for completeness in the migrate-specific test file).
func TestMigrateUp_NoMasterKey(t *testing.T) {
	t.Parallel()

	_, _, err := runCmd(t, "migrate", "up")
	if err == nil {
		t.Fatal("expected error when QOVIRA_MASTER_KEY is absent, got nil")
	}
	if !strings.Contains(err.Error(), "master_key") {
		t.Errorf("expected master_key error; got: %v", err)
	}
}

// ── AC: RunPostMigrateOptimize does not break the store ───────────────────────

// TestMigrateUp_OptimizeAfterMigrate verifies that after "migrate up" the store is still queryable
// (RunPostMigrateOptimize runs ANALYZE; this confirms it does not corrupt the database).
func TestMigrateUp_OptimizeAfterMigrate(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel().
	dataDir := t.TempDir()

	if _, stderr, err := runMigrateCmd(t, migrateEnv(dataDir), "migrate", "up"); err != nil {
		t.Fatalf("migrate up: %v\nstderr: %s", err, stderr)
	}

	s := openMigrateStore(t, dataDir)

	// A simple query proves the database is readable after Optimize.
	var n int
	if err := s.Writer().QueryRow(
		"SELECT count(*) FROM goose_db_version",
	).Scan(&n); err != nil {
		t.Fatalf("query after migrate up + optimize: %v", err)
	}
	if n == 0 {
		t.Error("goose_db_version is empty after migrate up; expected at least one row")
	}
}
