package store_test

import (
	"context"
	"testing"
	"time"
)

// TestHasAnyUser_EmptyDB verifies that HasAnyUser returns false on a freshly migrated database that contains no user
// rows.
func TestHasAnyUser_EmptyDB(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ctx := context.Background()

	got, err := s.HasAnyUser(ctx)
	if err != nil {
		t.Fatalf("HasAnyUser: unexpected error: %v", err)
	}
	if got {
		t.Error("HasAnyUser() = true on empty users table; want false")
	}
}

// TestHasAnyUser_AfterInsert verifies that HasAnyUser returns true after a user row has been inserted into the users
// table.
func TestHasAnyUser_AfterInsert(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ctx := context.Background()

	// Insert a minimal user row directly via the write pool so we don't need to import internal/auth here (avoids a
	// cycle and keeps the store test focused on the store layer).
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.Writer().ExecContext(ctx,
		`INSERT INTO users
		   (id, email, display_name, password_hash, role, timezone, locale, language, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"01JTEST00000000000000000001",
		"admin@example.com",
		"Admin",
		"$argon2id$v=19$m=65536,t=3,p=2$fakesalt$fakehash",
		"admin",
		"UTC",
		"en",
		"en",
		now,
		now,
	)
	if err != nil {
		t.Fatalf("insert user row: %v", err)
	}

	got, err := s.HasAnyUser(ctx)
	if err != nil {
		t.Fatalf("HasAnyUser: unexpected error: %v", err)
	}
	if !got {
		t.Error("HasAnyUser() = false after inserting a user row; want true")
	}
}

// TestHasAnyUser_ImplementsUserExisterSeam verifies that *store.Store satisfies the bootstrap.UserExister interface at
// compile time by using an interface conversion. The actual interface is defined in bootstrap; we replicate the minimal
// shape here to avoid importing bootstrap from the store test.
func TestHasAnyUser_ImplementsUserExisterSeam(t *testing.T) {
	t.Parallel()

	type userExister interface {
		HasAnyUser(ctx context.Context) (bool, error)
	}

	// This will not compile if *store.Store does not implement the seam.
	s := openMigratedStore(t)
	var _ userExister = s
}
