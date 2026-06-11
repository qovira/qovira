package store_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/store"
)

// openStore is a test helper that opens a store against a temp-dir database
// file and registers a Cleanup to close it.
func openStore(t *testing.T, cfg store.Config) *store.Store {
	t.Helper()
	s, err := store.Open(cfg)
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

// TestWrongKeyFails verifies AC1: opening with the wrong key returns an error
// at Open time, and opening with the right key succeeds and can read back data
// written in a previous open/close cycle.
func TestWrongKeyFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	correctKey := "a-sufficiently-long-passphrase-for-sqlcipher"
	wrongKey := "this-is-absolutely-the-wrong-key"

	// Phase 1: create the database and write a row.
	{
		s := openStore(t, store.Config{
			Path:         dbPath,
			Key:          correctKey,
			ReadPoolSize: 1,
		})
		_, err := s.Writer().Exec(
			"CREATE TABLE IF NOT EXISTS sentinel (id INTEGER PRIMARY KEY, val TEXT NOT NULL)",
		)
		if err != nil {
			t.Fatalf("create table: %v", err)
		}
		_, err = s.Writer().Exec("INSERT INTO sentinel (val) VALUES (?)", "hello")
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	// Phase 2: reopen with the correct key and verify the data survives.
	{
		s := openStore(t, store.Config{
			Path:         dbPath,
			Key:          correctKey,
			ReadPoolSize: 1,
		})
		var val string
		err := s.Reader().QueryRow("SELECT val FROM sentinel LIMIT 1").Scan(&val)
		if err != nil {
			t.Fatalf("read after reopen: %v", err)
		}
		if val != "hello" {
			t.Errorf("round-trip value = %q, want %q", val, "hello")
		}
	}

	// Phase 3: attempt to open with the wrong key — Open must return an error.
	_, err := store.Open(store.Config{
		Path:         dbPath,
		Key:          wrongKey,
		ReadPoolSize: 1,
	})
	if err == nil {
		t.Fatal("expected Open to fail with wrong key, but it succeeded")
	}
}

// TestPoolSizes verifies AC2: the write pool is capped at one connection and
// the read pool is capped at ReadPoolSize (both explicit and default).
func TestPoolSizes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		readPoolSize int
		wantRead     int
	}{
		{"explicit-4", 4, 4},
		{"explicit-8", 8, 8},
		{"default-zero", 0, 4}, // 0 → default (4)
		{"default-neg", -1, 4}, // negative → default (4)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s := openStore(t, store.Config{
				Path:         filepath.Join(dir, "test.db"),
				Key:          "a-sufficiently-long-passphrase-for-sqlcipher",
				ReadPoolSize: tt.readPoolSize,
			})

			if got := s.Writer().Stats().MaxOpenConnections; got != 1 {
				t.Errorf("write pool MaxOpenConnections = %d, want 1", got)
			}
			if got := s.Reader().Stats().MaxOpenConnections; got != tt.wantRead {
				t.Errorf("read pool MaxOpenConnections = %d, want %d", got, tt.wantRead)
			}
		})
	}
}

// TestPragmasApplied verifies AC3: the expected PRAGMAs are set on connections
// from both the write and read pools.
func TestPragmasApplied(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := openStore(t, store.Config{
		Path:         filepath.Join(dir, "test.db"),
		Key:          "a-sufficiently-long-passphrase-for-sqlcipher",
		ReadPoolSize: 2,
	})

	for _, tc := range []struct {
		name string
		db   *sql.DB
	}{
		{"writer", s.Writer()},
		{"reader", s.Reader()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// foreign_keys → 1
			assertPragmaInt(t, tc.db, "foreign_keys", 1)
			// journal_mode → wal (proves WAL PRAGMA ran; SQLite default is "delete")
			assertPragmaStr(t, tc.db, "journal_mode", "wal")
			// synchronous → 1 (NORMAL); proves the hook ran (SQLite default is 2 = FULL)
			assertPragmaInt(t, tc.db, "synchronous", 1)
			// busy_timeout → 5000 (design-mandated value; note: the driver's own
			// default is also 5000, so this assertion alone does not prove the
			// ConnectHook set it — the WAL and temp_store assertions above are what
			// actually demonstrate the hook executed on this connection).
			assertPragmaInt(t, tc.db, "busy_timeout", 5000)
			// temp_store → 2 (MEMORY); SQLite default is 0 (DEFAULT/FILE), so this
			// proves the hook ran even if busy_timeout coincidentally matches the default.
			assertPragmaInt(t, tc.db, "temp_store", 2)
		})
	}
}

// assertPragmaInt is a test helper that queries a PRAGMA and asserts the
// integer result equals want.
func assertPragmaInt(t *testing.T, db *sql.DB, pragma string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil {
		t.Fatalf("PRAGMA %s: %v", pragma, err)
	}
	if got != want {
		t.Errorf("PRAGMA %s = %d, want %d", pragma, got, want)
	}
}

// assertPragmaStr is a test helper that queries a PRAGMA and asserts the
// string result equals want.
func assertPragmaStr(t *testing.T, db *sql.DB, pragma string, want string) {
	t.Helper()
	var got string
	if err := db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil {
		t.Fatalf("PRAGMA %s: %v", pragma, err)
	}
	if got != want {
		t.Errorf("PRAGMA %s = %q, want %q", pragma, got, want)
	}
}

// TestConcurrentReadsWithWrite verifies AC4: multiple goroutines can read via
// the read pool while a write transaction is in progress on the write pool, and
// the race detector sees no data races.
func TestConcurrentReadsWithWrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := openStore(t, store.Config{
		Path:         filepath.Join(dir, "test.db"),
		Key:          "a-sufficiently-long-passphrase-for-sqlcipher",
		ReadPoolSize: 4,
	})

	// Set up a table with a few rows so reads have something real to do.
	_, err := s.Writer().Exec(
		"CREATE TABLE IF NOT EXISTS items (id INTEGER PRIMARY KEY, val INTEGER NOT NULL)",
	)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	for i := range 10 {
		if _, err := s.Writer().Exec("INSERT INTO items (val) VALUES (?)", i); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	// Begin a write transaction and hold it open for a moment.
	writeTx, err := s.Writer().Begin()
	if err != nil {
		t.Fatalf("begin write tx: %v", err)
	}

	writeHeld := make(chan struct{})
	writeRelease := make(chan struct{})

	go func() {
		if _, err := writeTx.Exec("INSERT INTO items (val) VALUES (999)"); err != nil {
			// Report but don't fatal from a goroutine.
			t.Errorf("write tx exec: %v", err)
		}
		close(writeHeld)
		<-writeRelease
		if err := writeTx.Commit(); err != nil {
			t.Errorf("write tx commit: %v", err)
		}
	}()

	// Wait for the write transaction to be in flight.
	select {
	case <-writeHeld:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for write transaction to start")
	}

	// Launch concurrent reads via the read pool — they must all succeed while
	// the write transaction is open.
	const numReaders = 8
	var wg sync.WaitGroup
	wg.Add(numReaders)
	for range numReaders {
		go func() {
			defer wg.Done()
			rows, err := s.Reader().Query("SELECT id, val FROM items ORDER BY id")
			if err != nil {
				t.Errorf("concurrent read: %v", err)
				return
			}
			defer func() {
				if cerr := rows.Close(); cerr != nil {
					t.Errorf("rows.Close: %v", cerr)
				}
			}()
			for rows.Next() {
				var id, val int
				if err := rows.Scan(&id, &val); err != nil {
					t.Errorf("scan: %v", err)
					return
				}
			}
			if err := rows.Err(); err != nil {
				t.Errorf("rows.Err: %v", err)
			}
		}()
	}
	wg.Wait()

	// Release the write transaction.
	close(writeRelease)
}

// TestEmptyKeyRejected verifies that Open with an empty encryption key
// must return a non-nil error and a nil Store. Without this guard the driver
// silently produces an unencrypted database, which is a silent security failure
// for an at-rest-encryption product.
func TestEmptyKeyRejected(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s, err := store.Open(store.Config{
		Path: filepath.Join(dir, "should-not-exist.db"),
		Key:  "",
	})
	if err == nil {
		// Close so we don't leak the handle before failing.
		if s != nil {
			_ = s.Close()
		}
		t.Fatal("Open with empty key must return an error, but it succeeded")
	}
	if s != nil {
		_ = s.Close()
		t.Errorf("Open with empty key returned a non-nil Store alongside the error; want nil Store")
	}
}

// TestSpecialCharPath verifies that database paths that contain
// characters reserved in SQLite URI filenames (#, space, %) are handled
// correctly. The test writes a row, closes, reopens, reads it back, and
// asserts the on-disk file exists at exactly the intended path.
func TestSpecialCharPath(t *testing.T) {
	t.Parallel()

	// Build a subpath inside TempDir whose directory and filename both contain
	// '#' (URI fragment delimiter), ' ' (space), and '%' (percent).
	// A literal '?' in the path is intentionally omitted: the driver splits the
	// DSN on the first '?' to find where query params begin, so a '?' in the
	// path is unrepresentable even with percent-encoding in the file: URI form.
	base := t.TempDir()
	weirdDir := filepath.Join(base, "weird #dir %x")
	if err := os.MkdirAll(weirdDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dbPath := filepath.Join(weirdDir, "qovira #test %20.db")
	const key = "a-sufficiently-long-passphrase-for-sqlcipher"

	// Phase 1: create the database and write a row.
	{
		s, err := store.Open(store.Config{Path: dbPath, Key: key, ReadPoolSize: 1})
		if err != nil {
			t.Fatalf("Open (phase 1) with special-char path: %v", err)
		}
		_, err = s.Writer().Exec(
			"CREATE TABLE IF NOT EXISTS sentinel (id INTEGER PRIMARY KEY, val TEXT NOT NULL)",
		)
		if err != nil {
			_ = s.Close()
			t.Fatalf("create table: %v", err)
		}
		_, err = s.Writer().Exec("INSERT INTO sentinel (val) VALUES (?)", "round-trip")
		if err != nil {
			_ = s.Close()
			t.Fatalf("insert: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close (phase 1): %v", err)
		}
	}

	// Verify the file was created at the intended path (not silently elsewhere).
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database not found at intended path %q: %v", dbPath, err)
	}

	// Phase 2: reopen and verify the data survives.
	{
		s, err := store.Open(store.Config{Path: dbPath, Key: key, ReadPoolSize: 1})
		if err != nil {
			t.Fatalf("Open (phase 2) with special-char path: %v", err)
		}
		defer func() {
			if err := s.Close(); err != nil {
				t.Errorf("close (phase 2): %v", err)
			}
		}()
		var val string
		err = s.Reader().QueryRow("SELECT val FROM sentinel LIMIT 1").Scan(&val)
		if err != nil {
			t.Fatalf("read after reopen: %v", err)
		}
		if val != "round-trip" {
			t.Errorf("round-trip value = %q, want %q", val, "round-trip")
		}
	}
}

// TestCloseSymmetric verifies that Close is symmetric — both the write and
// read pool errors, when present, are individually wrapped with their
// respective "store: close write pool" / "store: close read pool" prefixes and
// both are included in the returned error.
//
// database/sql.DB.Close() is idempotent (returns nil on a double-close), so
// we cannot trigger real pool-close errors through the public Store API. This
// test therefore verifies the happy path (a clean Close succeeds) and serves
// as a structural anchor documenting the intended behavior contract: both
// error branches wrap with fmt.Errorf("store: close X pool: %w", err) and are
// combined via errors.Join(werr, rerr) so both errors are always reported.
func TestCloseSymmetric(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s, err := store.Open(store.Config{
		Path:         filepath.Join(dir, "close-test.db"),
		Key:          "a-sufficiently-long-passphrase-for-sqlcipher",
		ReadPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Normal Close must succeed with no error.
	if err := s.Close(); err != nil {
		t.Errorf("Close: unexpected error: %v", err)
	}
}
