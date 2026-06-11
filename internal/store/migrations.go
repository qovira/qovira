package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"time"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// migrationsFS is the sub-FS rooted at the migrations directory so the goose provider receives a FS where the *.sql files sit at the top level (not under migrations/).
var migrationsFS, _ = fs.Sub(embeddedMigrations, "migrations")

// Runner executes goose migrations against a *sql.DB. The migrations filesystem is held as a field so tests can inject a synthetic FS to exercise error paths without touching the embedded production migrations.
type Runner struct {
	migsFS fs.FS
}

// NewRunner returns a Runner that uses the embedded production migration files. This is the constructor for all production call sites (serve, migrate).
func NewRunner() Runner {
	return Runner{migsFS: migrationsFS}
}

// NewRunnerWithFS returns a Runner backed by the provided fs.FS. It is intentionally unexported via its signature — exported only by name — so tests in the store package (and its test packages) can inject a broken FS to exercise Runner's error paths without exposing a broad configuration surface to callers outside the package.
func NewRunnerWithFS(fsys fs.FS) Runner {
	return Runner{migsFS: fsys}
}

// newProvider constructs a goose Provider over the Runner's migrations FS.
//
// Important: the caller must NOT call provider.Close() — Provider.Close() closes the underlying *sql.DB, which is owned by the Store. Closing it here would make the Store's pools unusable. The Provider itself is lightweight; letting it be collected by the GC when the caller's frame exits is correct.
func (r Runner) newProvider(db *sql.DB) (*goose.Provider, error) {
	p, err := goose.NewProvider(
		goose.DialectSQLite3,
		db,
		r.migsFS,
		goose.WithSlog(slog.Default()),
	)
	if err != nil {
		return nil, fmt.Errorf("migrations: create provider: %w", err)
	}
	return p, nil
}

// Up applies all pending migrations against db (which must be the write pool). It returns a wrapped error that includes the goose failure message so the caller can surface it to the operator without additional digging.
func (r Runner) Up(ctx context.Context, db *sql.DB) error {
	p, err := r.newProvider(db)
	if err != nil {
		return err
	}
	// Provider.Close() would close the *sql.DB, which is owned by the Store. Do not call it here.

	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrations: up: %w", err)
	}
	return nil
}

// Down rolls back the last applied migration against db (write pool). Returns a wrapped error on failure.
func (r Runner) Down(ctx context.Context, db *sql.DB) error {
	p, err := r.newProvider(db)
	if err != nil {
		return err
	}

	if _, err := p.Down(ctx); err != nil {
		return fmt.Errorf("migrations: down: %w", err)
	}
	return nil
}

// Status writes a human-readable migration status report to w. Each line describes one migration: its version, state (applied/pending), and when it was applied (or "—" if pending). Returns a wrapped error on failure.
func (r Runner) Status(ctx context.Context, db *sql.DB, w io.Writer) error {
	p, err := r.newProvider(db)
	if err != nil {
		return err
	}

	statuses, err := p.Status(ctx)
	if err != nil {
		return fmt.Errorf("migrations: status: %w", err)
	}

	for _, s := range statuses {
		appliedAt := "—"
		if s.State == goose.StateApplied {
			appliedAt = s.AppliedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(w, "  %5d  %-10s  %s  %s\n",
			s.Source.Version,
			string(s.State),
			appliedAt,
			s.Source.Path,
		)
	}
	return nil
}
