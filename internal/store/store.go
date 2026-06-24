// Package store handles SQLCipher database opening, and (in later work) goose migrations, sqlc-generated queries, and
// scoped data access.
//
// Opening: store.Open returns a *Store backed by two database/sql pools over the same encrypted SQLCipher file — one
// write pool capped at one connection (serialising all writes) and one read pool for concurrent reads via WAL
// snapshots.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	sqlite3 "github.com/omnilium/go-sqlcipher"
)

// defaultReadPoolSize is the number of read connections when Config.ReadPoolSize is not set (≤ 0). Four connections
// suit household-scale concurrency well; it is revisitable as load profiles become clearer.
const defaultReadPoolSize = 4

// optimizeInterval is how often the background goroutine runs PRAGMA optimize over the pools. Daily is appropriate for
// a household-scale daemon: it refreshes query-planner statistics without meaningfully loading the database.
const optimizeInterval = 24 * time.Hour

// driverName is the name under which the Qovira-specific SQLite driver is registered. A distinct name is required so
// that a ConnectHook (which applies per-connection PRAGMAs) can be attached without colliding with the default
// "sqlite3" driver registered by the library's own init().
const driverName = "qovira-sqlcipher"

// ErrWritePoolClose is the sentinel wrapped into the error returned by (*Store).Close when the write pool fails to
// close. Use errors.Is to detect it — Close wraps both the sentinel and the underlying error so both are identifiable
// after the call.
var ErrWritePoolClose = errors.New("store: close write pool")

// ErrReadPoolClose is the sentinel wrapped into the error returned by (*Store).Close when the read pool fails to close.
// Symmetric counterpart to ErrWritePoolClose.
var ErrReadPoolClose = errors.New("store: close read pool")

var registerOnce sync.Once

// register ensures the named driver is registered exactly once. sql.Register panics on duplicate registration, so the
// sync.Once guard is mandatory.
func register() {
	registerOnce.Do(func() {
		sql.Register(driverName, &sqlite3.SQLiteDriver{
			ConnectHook: applyPragmas,
		})
	})
}

// applyPragmas is the ConnectHook called by the driver for every new connection. The encryption key has already been
// applied via the _key DSN parameter (inside the driver's Open, before the first page read) — this hook handles the
// remaining per-connection PRAGMAs in the order mandated by the design:
//
//   - WAL → busy_timeout → foreign_keys → synchronous → temp_store: core correctness / durability settings.
//   - cache_size / mmap_size / journal_size_limit: performance baseline (§2.1 of the SQLite house guide).
//
// Note: PRAGMA optimize = 0x10002 (the per-connection planner-stats seed) is intentionally NOT run here. Running it
// inside the ConnectHook causes "database is locked" when a new read-pool connection is opened while a write
// transaction holds the WAL write lock — the optimize pragma internally acquires a shared lock that conflicts.
// Instead, Open runs PRAGMA optimize once after both pools are validated, and the background ticker re-runs it daily.
//
// foreign_keys in particular must be set per connection because SQLite defaults it to OFF and the setting is not
// persisted in the database file.
func applyPragmas(conn *sqlite3.SQLiteConn) error {
	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA foreign_keys = ON;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA temp_store = MEMORY;",
		// Performance baseline (house guide §2.1).
		"PRAGMA cache_size = -20000;",           // ~20 MiB page cache
		"PRAGMA mmap_size = 134217728;",         // 128 MiB memory-mapped I/O
		"PRAGMA journal_size_limit = 67108864;", // 64 MiB WAL size cap
	}
	for _, p := range pragmas {
		if _, err := conn.Exec(p, []driver.Value(nil)); err != nil {
			return fmt.Errorf("apply pragma %q: %w", p, err)
		}
	}
	return nil
}

// Config carries the parameters needed to open the encrypted database. It is intentionally narrow — the composition
// root maps internal/config values here; store does not import internal/config.
type Config struct {
	// Path is the filesystem path to the SQLCipher database file.
	Path string

	// Key is the master-key passphrase passed to SQLCipher's KDF. The caller is responsible for passing
	// string(cfg.MasterKey). This value is never logged.
	Key string

	// ReadPoolSize is the maximum number of concurrent read connections. A value ≤ 0 selects the built-in default (4).
	// This is intentionally configurable so operators with different concurrency profiles can tune it without a code
	// change.
	ReadPoolSize int
}

// Store wraps two database/sql pools over the same encrypted SQLCipher file. All mutation goes through the write pool;
// reads go through the read pool so long-running AI turns never stall behind a write.
//
// A background goroutine runs PRAGMA optimize over both pools on a daily timer (house guide §8.2) to keep query-planner
// statistics current. It is stopped cleanly by Close.
type Store struct {
	writeDB *sql.DB
	readDB  *sql.DB
	// stopOptimize is closed by Close to signal the optimize goroutine to exit.
	stopOptimize chan struct{}
	// optimizeDone is closed by the optimize goroutine once it has exited.
	optimizeDone chan struct{}
}

// Writer returns the write pool. It is capped at one connection and should be used for all INSERT/UPDATE/DELETE
// statements and explicit transactions. The write pool's DSN carries _txlock=immediate so every BeginTx uses BEGIN
// IMMEDIATE, which takes the write lock up front and lets busy_timeout apply correctly.
func (s *Store) Writer() *sql.DB { return s.writeDB }

// Reader returns the read pool. It is configured for concurrent reads via WAL snapshots and should be used for
// SELECT-only workloads.
func (s *Store) Reader() *sql.DB { return s.readDB }

// Close closes both pools and returns a joined error if either or both fail. Both errors are always reported so the
// caller can distinguish a write-pool failure (which may indicate uncheckpointed WAL data) from a read-pool failure.
// The errors are independently wrapped so they remain identifiable after errors.Join.
//
// A write-pool failure is detectable via errors.Is(err, ErrWritePoolClose); a read-pool failure via errors.Is(err,
// ErrReadPoolClose). Each wraps both the sentinel and the underlying error (Go 1.20+ multi-%w) so the root cause is
// accessible via errors.Unwrap.
//
// Close is idempotent for sequential callers: repeated calls are safe (database/sql.DB.Close is idempotent, and the
// stop channel's close is guarded by a select/default so a second sequential Close does not re-close it). Close is not
// safe to call concurrently from multiple goroutines; no caller does.
func (s *Store) Close() error {
	// Signal and wait for the optimize goroutine before closing the pools it uses. The select/default
	// guards against re-closing stopOptimize on a second sequential Close. A concurrent double-Close
	// (two goroutines racing on the default arm) is not supported and does not occur.
	select {
	case <-s.stopOptimize:
		// Already closed — second call, skip.
	default:
		close(s.stopOptimize)
	}
	<-s.optimizeDone

	writeErr := s.writeDB.Close()
	readErr := s.readDB.Close()

	var werr, rerr error
	if writeErr != nil {
		werr = fmt.Errorf("%w: %w", ErrWritePoolClose, writeErr)
	}
	if readErr != nil {
		rerr = fmt.Errorf("%w: %w", ErrReadPoolClose, readErr)
	}
	return errors.Join(werr, rerr)
}

// Open opens the encrypted SQLCipher file at cfg.Path and returns a *Store backed by a write pool (MaxOpenConns=1) and
// a read pool (MaxOpenConns=ReadPoolSize). The encryption key is applied before the first page read by the driver; a
// wrong key is detected eagerly (the first SELECT against sqlite_master will surface the error).
//
// Open validates the key immediately: it pings the write pool and runs a light read against sqlite_master so that a
// wrong key fails at call time rather than lazily on the first query. The write pool is validated and WAL mode is
// established before the read connections are created.
//
// After migrations the caller should run PRAGMA optimize (no argument) once; the periodic background goroutine handles
// all subsequent refreshes. See RunPostMigrateOptimize for the post-migration step.
func Open(cfg Config) (*Store, error) {
	// Reject an empty key up front. The go-sqlcipher driver only applies PRAGMA key when _key is non-empty, so an empty
	// key silently produces a plaintext, unencrypted database — a critical security failure for an at-rest-encryption
	// product.
	if cfg.Key == "" {
		return nil, errors.New("store: encryption key is required")
	}

	// Create the parent directory if it does not exist. SQLite's open call does not create parent directories; a
	// missing dir causes an "unable to open database file" error on a clean install. 0o700 is used because this
	// directory holds the encrypted database and should not be world-readable.
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o700); err != nil {
		return nil, fmt.Errorf("store: create data directory: %w", err)
	}

	register()

	readPoolSize := cfg.ReadPoolSize
	if readPoolSize <= 0 {
		readPoolSize = defaultReadPoolSize
	}

	// The write pool uses _txlock=immediate so every BeginTx translates to BEGIN IMMEDIATE,
	// which takes the write lock up front. This avoids the DEFERRED read→write lock-upgrade
	// that can hit an un-retryable SQLITE_BUSY that busy_timeout cannot catch (house guide §6.2).
	writeDSN := buildWriteDSN(cfg.Path, cfg.Key)
	readDSN := buildReadDSN(cfg.Path, cfg.Key)

	// Open and validate the write pool first. This establishes WAL mode before any read connection attaches; SQLite
	// requires WAL to be set by a writer.
	writeDB, err := openPool(writeDSN, 1)
	if err != nil {
		return nil, fmt.Errorf("store: open write pool: %w", err)
	}

	if err := validatePool(writeDB); err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("store: validate write pool: %w", err)
	}

	// Open the read pool once WAL is in place.
	readDB, err := openPool(readDSN, readPoolSize)
	if err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("store: open read pool: %w", err)
	}

	if err := validatePool(readDB); err != nil {
		_ = writeDB.Close()
		_ = readDB.Close()
		return nil, fmt.Errorf("store: validate read pool: %w", err)
	}

	// Seed query-planner statistics on each pool once, now that both pools are open and no
	// transaction is in flight. PRAGMA optimize = 0x10002 is the at-open invocation SQLite
	// recommends for long-lived connections (house guide §2.3 / §8.2); it must run outside any
	// transaction. We run it on the write pool first, then on the read pool.
	seedOptimize(writeDB)
	seedOptimize(readDB)

	stopOptimize := make(chan struct{})
	optimizeDone := make(chan struct{})

	st := &Store{
		writeDB:      writeDB,
		readDB:       readDB,
		stopOptimize: stopOptimize,
		optimizeDone: optimizeDone,
	}

	// Start the background goroutine that runs PRAGMA optimize periodically (house guide §8.2).
	go runOptimizeLoop(writeDB, readDB, stopOptimize, optimizeDone)

	return st, nil
}

// RunPostMigrateOptimize runs PRAGMA optimize (no argument) on the write pool once, immediately. Call this after
// migrations complete so the query planner has current statistics for any indexes just created.
func RunPostMigrateOptimize(ctx context.Context, db *sql.DB) {
	if _, err := db.ExecContext(ctx, "PRAGMA optimize;"); err != nil {
		slog.Warn("store: post-migrate optimize failed", "err", err)
	}
}

// runOptimizeLoop runs PRAGMA optimize over writeDB and readDB on a daily ticker until stopOptimize is closed, then
// closes optimizeDone to signal that the goroutine has exited.
func runOptimizeLoop(writeDB, readDB *sql.DB, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)

	ticker := time.NewTicker(optimizeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			runOptimize(writeDB)
			runOptimize(readDB)
		}
	}
}

// runOptimize runs PRAGMA optimize on db, logging a warning if it fails. It is a best-effort maintenance step and
// should never block callers.
func runOptimize(db *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "PRAGMA optimize;"); err != nil {
		slog.Warn("store: periodic optimize failed", "err", err)
	}
}

// seedOptimize runs PRAGMA optimize = 0x10002 on db once at pool-open time — the invocation SQLite recommends for a
// long-lived connection when it first opens (house guide §2.3 / SQLite docs). It primes the planner's statistics; the
// periodic no-argument PRAGMA optimize in runOptimizeLoop keeps them fresh thereafter. PRAGMA optimize must run outside
// any transaction, so this is called after both pools are validated and no transaction is in flight.
func seedOptimize(db *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "PRAGMA optimize = 0x10002;"); err != nil {
		slog.Warn("store: seed optimize failed", "err", err)
	}
}

// buildWriteDSN constructs the DSN for the write pool. It carries _txlock=immediate so every BeginTx on the write pool
// uses BEGIN IMMEDIATE, taking the write lock up front rather than on the first write inside a deferred transaction.
func buildWriteDSN(path, key string) string {
	params := url.Values{}
	params.Set("_key", key)
	params.Set("_txlock", "immediate")
	return buildDSNFromParams(path, params)
}

// buildReadDSN constructs the DSN for the read pool. The read pool stays DEFERRED (no _txlock) because it is
// SELECT-only and never needs a write lock.
func buildReadDSN(path, key string) string {
	params := url.Values{}
	params.Set("_key", key)
	return buildDSNFromParams(path, params)
}

// buildDSNFromParams constructs a SQLite URI DSN from a path and an already-populated url.Values.
//
// Path encoding: SQLite's URI parser (which receives the full DSN when the scheme is "file:") treats '#' as a fragment
// delimiter and '?' as the start of the query string, and interprets '%xx' percent-escape sequences in the path
// segment. Raw concatenation therefore silently misroutes paths that contain any of those bytes. The fix is to
// percent-encode the path before embedding it in the URI, using url.URL so the standard library handles all reserved
// characters correctly (%, #, ?, space, …).
//
// Note on '?' in the path: the driver splits on the first literal '?' byte (strings.IndexRune) to locate its query
// parameters before handing the full URI to sqlite3_open_v2. Because url.URL encodes a path-segment '?' as '%3F' — not
// a literal '?' — the driver's split still lands correctly on the query-string delimiter. SQLite's URI parser then
// decodes '%3F' back to '?' in the actual filename, so '?' in a path works with this encoding.
func buildDSNFromParams(path string, params url.Values) string {
	u := &url.URL{
		Scheme:   "file",
		OmitHost: true,
		Path:     path,
		RawQuery: params.Encode(),
	}
	return u.String()
}

// TestOnlyDSNs returns the write-pool DSN and read-pool DSN for the given path and key. It is exported solely for
// white-box testing of the DSN construction logic (finding 3: write pool must carry _txlock=immediate, read pool must
// not). Do not use in production code.
func TestOnlyDSNs(path, key string) (writeDSN, readDSN string) {
	return buildWriteDSN(path, key), buildReadDSN(path, key)
}

// openPool opens a database/sql pool against dsn with the named driver and sets MaxOpenConns. sql.Open is lazy — no
// real connection is made here.
func openPool(dsn string, maxConns int) (*sql.DB, error) {
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxConns)
	return db, nil
}

// validatePool forces a real connection and a page read so that a wrong key (or a corrupt/missing file) surfaces
// immediately. SELECT count(*) FROM sqlite_master touches the schema page, which is the first page SQLCipher decrypts;
// a wrong key causes an error here.
func validatePool(db *sql.DB) error {
	if err := db.Ping(); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&n); err != nil {
		return fmt.Errorf("read sqlite_master: %w", err)
	}
	return nil
}
