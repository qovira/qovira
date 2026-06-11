// Package store handles SQLCipher database opening, and (in later work)
// goose migrations, sqlc-generated queries, and scoped data access.
//
// Opening: store.Open returns a *Store backed by two database/sql pools over
// the same encrypted SQLCipher file — one write pool capped at one connection
// (serialising all writes) and one read pool for concurrent reads via WAL
// snapshots.
package store

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"sync"

	sqlite3 "github.com/omnilium/go-sqlcipher"
)

// defaultReadPoolSize is the number of read connections when Config.ReadPoolSize
// is not set (≤ 0). Four connections suit household-scale concurrency well; it
// is revisitable as load profiles become clearer.
const defaultReadPoolSize = 4

// driverName is the name under which the Qovira-specific SQLite driver is
// registered. A distinct name is required so that a ConnectHook (which applies
// per-connection PRAGMAs) can be attached without colliding with the default
// "sqlite3" driver registered by the library's own init().
const driverName = "qovira-sqlcipher"

var registerOnce sync.Once

// register ensures the named driver is registered exactly once. sql.Register
// panics on duplicate registration, so the sync.Once guard is mandatory.
func register() {
	registerOnce.Do(func() {
		sql.Register(driverName, &sqlite3.SQLiteDriver{
			ConnectHook: applyPragmas,
		})
	})
}

// applyPragmas is the ConnectHook called by the driver for every new
// connection. The encryption key has already been applied via the _key DSN
// parameter (inside the driver's Open, before the first page read) — this hook
// handles the remaining per-connection PRAGMAs in the order mandated by the
// design: WAL → busy_timeout → foreign_keys → synchronous → temp_store.
//
// foreign_keys in particular must be set per connection because SQLite defaults
// it to OFF and the setting is not persisted in the database file.
func applyPragmas(conn *sqlite3.SQLiteConn) error {
	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA foreign_keys = ON;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA temp_store = MEMORY;",
	}
	for _, p := range pragmas {
		if _, err := conn.Exec(p, []driver.Value(nil)); err != nil {
			return fmt.Errorf("apply pragma %q: %w", p, err)
		}
	}
	return nil
}

// Config carries the parameters needed to open the encrypted database. It is
// intentionally narrow — the composition root maps internal/config values
// here; store does not import internal/config.
type Config struct {
	// Path is the filesystem path to the SQLCipher database file.
	Path string

	// Key is the master-key passphrase passed to SQLCipher's KDF. The caller
	// is responsible for passing string(cfg.MasterKey). This value is never
	// logged.
	Key string

	// ReadPoolSize is the maximum number of concurrent read connections. A
	// value ≤ 0 selects the built-in default (4). This is intentionally
	// configurable so operators with different concurrency profiles can tune it
	// without a code change.
	ReadPoolSize int
}

// Store wraps two database/sql pools over the same encrypted SQLCipher file.
// All mutation goes through the write pool; reads go through the read pool so
// long-running AI turns never stall behind a write.
type Store struct {
	writeDB *sql.DB
	readDB  *sql.DB
}

// Writer returns the write pool. It is capped at one connection and should be
// used for all INSERT/UPDATE/DELETE statements and explicit transactions.
func (s *Store) Writer() *sql.DB { return s.writeDB }

// Reader returns the read pool. It is configured for concurrent reads via WAL
// snapshots and should be used for SELECT-only workloads.
func (s *Store) Reader() *sql.DB { return s.readDB }

// Close closes both pools and returns a joined error if either or both fail.
// Both errors are always reported so the caller can distinguish a write-pool
// failure (which may indicate uncheckpointed WAL data) from a read-pool
// failure. The errors are independently wrapped so they remain identifiable
// after errors.Join.
func (s *Store) Close() error {
	writeErr := s.writeDB.Close()
	readErr := s.readDB.Close()

	var werr, rerr error
	if writeErr != nil {
		werr = fmt.Errorf("store: close write pool: %w", writeErr)
	}
	if readErr != nil {
		rerr = fmt.Errorf("store: close read pool: %w", readErr)
	}
	return errors.Join(werr, rerr)
}

// Open opens the encrypted SQLCipher file at cfg.Path and returns a *Store
// backed by a write pool (MaxOpenConns=1) and a read pool
// (MaxOpenConns=ReadPoolSize). The encryption key is applied before the first
// page read by the driver; a wrong key is detected eagerly (the first SELECT
// against sqlite_master will surface the error).
//
// Open validates the key immediately: it pings the write pool and runs a
// light read against sqlite_master so that a wrong key fails at call time
// rather than lazily on the first query. The write pool is validated and WAL
// mode is established before the read connections are created.
func Open(cfg Config) (*Store, error) {
	// Reject an empty key up front. The go-sqlcipher driver only applies
	// PRAGMA key when _key is non-empty, so an empty key silently produces a
	// plaintext, unencrypted database — a critical security failure for an
	// at-rest-encryption product.
	if cfg.Key == "" {
		return nil, errors.New("store: encryption key is required")
	}

	register()

	readPoolSize := cfg.ReadPoolSize
	if readPoolSize <= 0 {
		readPoolSize = defaultReadPoolSize
	}

	dsn := buildDSN(cfg.Path, cfg.Key)

	// Open and validate the write pool first. This establishes WAL mode before
	// any read connection attaches; SQLite requires WAL to be set by a writer.
	writeDB, err := openPool(dsn, 1)
	if err != nil {
		return nil, fmt.Errorf("store: open write pool: %w", err)
	}

	if err := validatePool(writeDB); err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("store: validate write pool: %w", err)
	}

	// Open the read pool once WAL is in place.
	readDB, err := openPool(dsn, readPoolSize)
	if err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("store: open read pool: %w", err)
	}

	if err := validatePool(readDB); err != nil {
		_ = writeDB.Close()
		_ = readDB.Close()
		return nil, fmt.Errorf("store: validate read pool: %w", err)
	}

	return &Store{writeDB: writeDB, readDB: readDB}, nil
}

// buildDSN constructs a SQLite URI DSN with the _key query parameter.
//
// Path encoding: SQLite's URI parser (which receives the full DSN when the
// scheme is "file:") treats '#' as a fragment delimiter and '?' as the start
// of the query string, and interprets '%xx' percent-escape sequences in the
// path segment. Raw concatenation therefore silently misroutes paths that
// contain any of those bytes. The fix is to percent-encode the path before
// embedding it in the URI, using url.URL so the standard library handles all
// reserved characters correctly (%, #, ?, space, …).
//
// The _key value is encoded via url.Values so special characters in the
// passphrase are also safe. The DSN itself is never logged.
//
// Note on '?' in the path: the driver splits on the first literal '?' byte
// (strings.IndexRune) to locate its query parameters before handing the full
// URI to sqlite3_open_v2. Because url.URL encodes a path-segment '?' as
// '%3F' — not a literal '?' — the driver's split still lands correctly on the
// query-string delimiter. SQLite's URI parser then decodes '%3F' back to '?'
// in the actual filename, so '?' in a path works with this encoding. It is
// listed here for completeness; callers should nonetheless prefer '?' -free
// paths for maximum portability across file-systems and tools.
func buildDSN(path, key string) string {
	params := url.Values{}
	params.Set("_key", key)
	// url.URL.EscapedPath() / url.PathEscape() percent-encodes reserved
	// characters (including '#', '%', and space) that would otherwise be
	// mis-parsed by SQLite's URI filename parser.
	u := &url.URL{
		Scheme:   "file",
		OmitHost: true,
		Path:     path,
		RawQuery: params.Encode(),
	}
	return u.String()
}

// openPool opens a database/sql pool against dsn with the named driver and
// sets MaxOpenConns. sql.Open is lazy — no real connection is made here.
func openPool(dsn string, maxConns int) (*sql.DB, error) {
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxConns)
	return db, nil
}

// validatePool forces a real connection and a page read so that a wrong key
// (or a corrupt/missing file) surfaces immediately. SELECT count(*) FROM
// sqlite_master touches the schema page, which is the first page SQLCipher
// decrypts; a wrong key causes an error here.
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
