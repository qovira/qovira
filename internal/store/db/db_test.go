// Package db_test exercises the sqlc-generated queries against a real
// SQLCipher database to verify that the generated code compiles and works
// end-to-end (acceptance criterion 5).
package db_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/qovira/qovira/internal/store"
	"github.com/qovira/qovira/internal/store/db"
)

const testKey = "a-sufficiently-long-passphrase-for-sqlcipher"

// TestGetInstance_WriterPool verifies that after migrations are applied, the
// migration-inserted instance row is readable via GetInstance on the write
// pool, with a non-empty created_at timestamp. No app-level insert is needed.
func TestGetInstance_WriterPool(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
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

	// Apply migrations so the instance table exists and has the seed row.
	runner := store.NewRunner()
	if err := runner.Up(context.Background(), s.Writer()); err != nil {
		t.Fatalf("runner.Up: %v", err)
	}

	ctx := context.Background()
	q := db.New(s.Writer())

	inst, err := q.GetInstance(ctx)
	if err != nil {
		t.Fatalf("GetInstance via writer: %v", err)
	}

	if inst.ID != 1 {
		t.Errorf("instance.ID = %d, want 1", inst.ID)
	}
	if inst.CreatedAt == "" {
		t.Error("instance.CreatedAt is empty; migration should have populated it via DEFAULT")
	}

	// Verify the Querier interface is satisfied by *db.Queries (compile-time
	// assertion lives in querier.go; this confirms it at runtime too).
	var _ db.Querier = q
}

// TestGetInstance_ReaderPool verifies that after migrations are applied, the
// migration-inserted instance row is also readable via the read pool. This
// confirms the write-pool-only migration requirement: the schema and data
// written through the write pool are visible to the separate read pool via WAL.
func TestGetInstance_ReaderPool(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
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

	runner := store.NewRunner()
	if err := runner.Up(context.Background(), s.Writer()); err != nil {
		t.Fatalf("runner.Up: %v", err)
	}

	ctx := context.Background()

	// Read via read pool — must see the migration-inserted row.
	rq := db.New(s.Reader())
	inst, err := rq.GetInstance(ctx)
	if err != nil {
		t.Fatalf("GetInstance via reader: %v", err)
	}
	if inst.ID != 1 {
		t.Errorf("read-pool instance.ID = %d, want 1", inst.ID)
	}
	if inst.CreatedAt == "" {
		t.Error("read-pool instance.CreatedAt is empty; migration should have populated it via DEFAULT")
	}
}
