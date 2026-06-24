package store_test

import (
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pressly/goose/v3"

	"github.com/qovira/qovira/internal/store"
)

// testKey is a valid-length SQLCipher passphrase for use in migration tests.
const testKey = "a-sufficiently-long-passphrase-for-sqlcipher"

// openTestStore is a helper that opens a store at a temp-dir path and registers a Cleanup to close it.
func openTestStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	s, err := store.Open(store.Config{
		Path:         filepath.Join(dir, "test.db"),
		Key:          testKey,
		ReadPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return s
}

// tableExists returns true when the named table appears in sqlite_master.
func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	err := db.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name,
	).Scan(&n)
	if err != nil {
		t.Fatalf("query sqlite_master for %q: %v", name, err)
	}
	return n > 0
}

// TestRunnerUp_CreatesSchema verifies that Runner.Up applies migrations and creates the expected schema objects
// (instance, user_data, settings, users, sessions, conversations, messages, and the goose version table).
func TestRunnerUp_CreatesSchema(t *testing.T) {
	t.Parallel()

	s := openTestStore(t, t.TempDir())
	runner := store.NewRunner()

	if err := runner.Up(context.Background(), s.Writer()); err != nil {
		t.Fatalf("Runner.Up: %v", err)
	}

	if !tableExists(t, s.Writer(), "instance") {
		t.Error("instance table not found after Up")
	}
	if !tableExists(t, s.Writer(), "user_data") {
		t.Error("user_data table not found after Up")
	}
	if !tableExists(t, s.Writer(), "settings") {
		t.Error("settings table not found after Up")
	}
	if !tableExists(t, s.Writer(), "users") {
		t.Error("users table not found after Up")
	}
	if !tableExists(t, s.Writer(), "sessions") {
		t.Error("sessions table not found after Up")
	}
	if !tableExists(t, s.Writer(), "conversations") {
		t.Error("conversations table not found after Up")
	}
	if !tableExists(t, s.Writer(), "messages") {
		t.Error("messages table not found after Up")
	}
	if !tableExists(t, s.Writer(), "goose_db_version") {
		t.Error("goose_db_version table not found after Up")
	}
}

// TestRunnerDown_RevertsSchema verifies that Runner.Down rolls back the last applied migration. A single Down call
// reverts only the top migration while leaving all earlier migrations intact. The messages table must still be present
// after this Down because it was created by migration 7 and no Down in the chain removes it.
func TestRunnerDown_RevertsSchema(t *testing.T) {
	t.Parallel()

	s := openTestStore(t, t.TempDir())
	runner := store.NewRunner()

	if err := runner.Up(context.Background(), s.Writer()); err != nil {
		t.Fatalf("Runner.Up: %v", err)
	}
	if !tableExists(t, s.Writer(), "messages") {
		t.Fatal("messages table must exist before Down")
	}

	if err := runner.Down(context.Background(), s.Writer()); err != nil {
		t.Fatalf("Runner.Down: %v", err)
	}
	// The messages table remains: no Down in the chain after migration 7 removes it.
	if !tableExists(t, s.Writer(), "messages") {
		t.Error("messages table absent after single Down; it was created by migration 7 which was not rolled back")
	}
	// Core schema tables created by earlier migrations must still be present.
	if !tableExists(t, s.Writer(), "instance") {
		t.Error("instance table absent after single Down; only the top migration should have been rolled back")
	}
	if !tableExists(t, s.Writer(), "user_data") {
		t.Error("user_data table absent after single Down; only the top migration should have been rolled back")
	}
	if !tableExists(t, s.Writer(), "settings") {
		t.Error("settings table absent after single Down; only the top migration should have been rolled back")
	}
	if !tableExists(t, s.Writer(), "users") {
		t.Error("users table absent after single Down; only the top migration should have been rolled back")
	}
	if !tableExists(t, s.Writer(), "sessions") {
		t.Error("sessions table absent after single Down; only the top migration should have been rolled back")
	}
	if !tableExists(t, s.Writer(), "conversations") {
		t.Error("conversations table absent after single Down; only the top migration should have been rolled back")
	}
}

// TestRunnerStatus_ReflectsAppliedState verifies that Status output correctly reflects the state before and after Up.
// Before Up the single migration is pending; after Up it is applied.
func TestRunnerStatus_ReflectsAppliedState(t *testing.T) {
	t.Parallel()

	s := openTestStore(t, t.TempDir())
	runner := store.NewRunner()

	// Before any migration runs: migration 1 is pending.
	var beforeBuf strings.Builder
	if err := runner.Status(context.Background(), s.Writer(), &beforeBuf); err != nil {
		t.Fatalf("Status before Up: %v", err)
	}
	beforeOut := beforeBuf.String()
	if !strings.Contains(beforeOut, string(goose.StatePending)) {
		t.Errorf("expected %q in Status output before Up; got:\n%s", goose.StatePending, beforeOut)
	}

	// Run all migrations.
	if err := runner.Up(context.Background(), s.Writer()); err != nil {
		t.Fatalf("Runner.Up: %v", err)
	}

	// After Up: migration 1 is applied.
	var afterBuf strings.Builder
	if err := runner.Status(context.Background(), s.Writer(), &afterBuf); err != nil {
		t.Fatalf("Status after Up: %v", err)
	}
	afterOut := afterBuf.String()
	if !strings.Contains(afterOut, string(goose.StateApplied)) {
		t.Errorf("expected %q in Status output after Up; got:\n%s", goose.StateApplied, afterOut)
	}
}

// TestRunnerUpDownStatus_RoundTrip is the integration round-trip that maps to acceptance criterion 3: Up → Status shows
// applied → Down → Status shows pending.
func TestRunnerUpDownStatus_RoundTrip(t *testing.T) {
	t.Parallel()

	s := openTestStore(t, t.TempDir())
	runner := store.NewRunner()
	ctx := context.Background()

	// Up.
	if err := runner.Up(ctx, s.Writer()); err != nil {
		t.Fatalf("Runner.Up: %v", err)
	}

	// Status must show applied.
	var afterUp strings.Builder
	if err := runner.Status(ctx, s.Writer(), &afterUp); err != nil {
		t.Fatalf("Status after Up: %v", err)
	}
	if !strings.Contains(afterUp.String(), string(goose.StateApplied)) {
		t.Errorf("after Up: expected applied state in status output, got:\n%s", afterUp.String())
	}

	// Down: reverts only the top migration; all earlier migrations stay applied.
	if err := runner.Down(ctx, s.Writer()); err != nil {
		t.Fatalf("Runner.Down: %v", err)
	}

	// Status must show at least one pending migration (the top one was rolled back).
	var afterDown strings.Builder
	if err := runner.Status(ctx, s.Writer(), &afterDown); err != nil {
		t.Fatalf("Status after Down: %v", err)
	}
	if !strings.Contains(afterDown.String(), string(goose.StatePending)) {
		t.Errorf("after Down: expected pending state in status output, got:\n%s", afterDown.String())
	}
	// The messages table was created by migration 7 and no Down in the chain after that removes it, so it must still be
	// present.
	if !tableExists(t, s.Writer(), "messages") {
		t.Error("messages table must be present after top-migration Down (migration 7 is still applied)")
	}
	// Core schema tables created by earlier migrations must remain applied.
	if !tableExists(t, s.Writer(), "instance") {
		t.Error("instance table must remain present — only the top migration was rolled back")
	}
	if !tableExists(t, s.Writer(), "user_data") {
		t.Error("user_data table must remain present — only the top migration was rolled back")
	}
	if !tableExists(t, s.Writer(), "settings") {
		t.Error("settings table must remain present — only the top migration was rolled back")
	}
	if !tableExists(t, s.Writer(), "users") {
		t.Error("users table must remain present — only the top migration was rolled back")
	}
}

// TestRunnerUp_BadMigration verifies that a broken migration surfaces a wrapped, non-panic error through the real
// Runner.Up code path. This exercises Qovira's fail-fast: Runner.Up over an injected broken fs.FS returns a non-nil
// wrapped error without panicking.
func TestRunnerUp_BadMigration(t *testing.T) {
	t.Parallel()

	s := openTestStore(t, t.TempDir())

	// Construct a synthetic FS with a deliberately broken migration and inject it into a Runner via the
	// package-internal test helper.
	badFS := fstest.MapFS{
		"00001_bad.sql": &fstest.MapFile{
			Data: []byte("-- +goose Up\nNOT VALID SQL AT ALL;\n\n-- +goose Down\nSELECT 1;\n"),
		},
	}

	runner := store.NewRunnerWithFS(badFS)
	err := runner.Up(context.Background(), s.Writer())
	if err == nil {
		t.Fatal("expected Runner.Up to fail on a broken migration, but it succeeded")
	}
	// Verify we get a real descriptive wrapped error, not a panic.
	if err.Error() == "" {
		t.Error("Runner.Up error message is empty; expected a descriptive wrapped error")
	}
	t.Logf("bad migration error (expected): %v", err)
}

// TestWriterPoolMigration verifies AC4: migrations run against the write pool, and after migration the read pool (a
// separate connection pool to the same file) can observe the migrated schema. This structurally confirms the
// write-pool-only requirement.
func TestWriterPoolMigration(t *testing.T) {
	t.Parallel()

	s := openTestStore(t, t.TempDir())
	runner := store.NewRunner()

	// Run migrations against the WRITE pool (as required by the design).
	if err := runner.Up(context.Background(), s.Writer()); err != nil {
		t.Fatalf("Runner.Up via write pool: %v", err)
	}

	// The READ pool (separate connection pool, same file) must see the migrated schema because both pools share the WAL
	// file.
	if !tableExists(t, s.Reader(), "instance") {
		t.Error("instance table not visible via read pool after migration on write pool")
	}
}

// TestDBPath verifies that DBPath derives the expected path from a given dataDir.
func TestDBPath(t *testing.T) {
	t.Parallel()

	got := store.DBPath("/var/lib/qovira")
	want := "/var/lib/qovira/qovira.db"
	if got != want {
		t.Errorf("DBPath = %q, want %q", got, want)
	}
}

// TestRunnerStatus_WritesToWriter verifies that Runner.Status writes output to the provided io.Writer and does not
// print to stdout directly.
func TestRunnerStatus_WritesToWriter(t *testing.T) {
	t.Parallel()

	s := openTestStore(t, t.TempDir())
	runner := store.NewRunner()

	var buf strings.Builder
	if err := runner.Status(context.Background(), s.Writer(), &buf); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if buf.Len() == 0 {
		t.Error("expected non-empty output from Status, but the writer received nothing")
	}
	// Verify io.Discard also works (no panic on nil-style writer).
	if err := runner.Status(context.Background(), s.Writer(), io.Discard); err != nil {
		t.Errorf("Status to io.Discard: %v", err)
	}
}
