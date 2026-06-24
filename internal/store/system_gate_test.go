package store

// Whitebox tests for the system-scope gate on the five scope-bypass methods. These live in package store (not
// store_test) so they can reference the unexported errUserScopeForSystemMethod sentinel.
//
// Each method must reject a user scope with errUserScopeForSystemMethod before any DB work and let a system scope
// through.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// openMigratedStoreInternal opens a migrated store for whitebox tests. Mirrors openMigratedStore in
// scoped_queries_test.go but lives in package store.
func openMigratedStoreInternal(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(Config{
		Path:         filepath.Join(dir, "test.db"),
		Key:          "a-sufficiently-long-passphrase-for-sqlcipher",
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
	runner := NewRunner()
	if err := runner.Up(context.Background(), s.Writer()); err != nil {
		t.Fatalf("runner.Up: %v", err)
	}
	return s
}

// userSQ returns a user-scoped *ScopedQueries for gate rejection tests.
func userSQ(s *Store) *ScopedQueries {
	return s.ForUser(UserScope(Principal{UserID: "gate-test-user", Role: "member"}))
}

// sysSQ returns a system-scoped *ScopedQueries for gate pass-through tests.
func sysSQInternal(s *Store) *ScopedQueries {
	return s.ForUser(SystemScope())
}

// TestSystemGate_InsertMessageByUserID verifies that InsertMessageByUserID rejects a user scope with
// errUserScopeForSystemMethod and passes a system scope.
func TestSystemGate_InsertMessageByUserID(t *testing.T) {
	t.Parallel()
	s := openMigratedStoreInternal(t)
	ctx := context.Background()

	t.Run("user_scope_rejected", func(t *testing.T) {
		sq := userSQ(s)
		err := sq.InsertMessageByUserID(ctx, "id1", "conv1", "user1", "call1", "content")
		if !errors.Is(err, errUserScopeForSystemMethod) {
			t.Errorf("user scope: want errUserScopeForSystemMethod; got: %v", err)
		}
	})

	t.Run("system_scope_passes_gate", func(t *testing.T) {
		// The system scope should get past the gate (may still fail on FK constraints since we haven't seeded the
		// DB — that's fine, any error other than the gate sentinel means the gate itself passed).
		sq := sysSQInternal(s)
		err := sq.InsertMessageByUserID(ctx, "id1", "conv-nonexistent", "user-none", "call1", "content")
		if errors.Is(err, errUserScopeForSystemMethod) {
			t.Errorf("system scope: must not return errUserScopeForSystemMethod; got: %v", err)
		}
		// Any other error (FK violation, etc.) is acceptable — gate passed.
	})
}

// TestSystemGate_CountNonExpiredConfirmationsByMessageIDForUser verifies the gate on
// CountNonExpiredConfirmationsByMessageIDForUser.
func TestSystemGate_CountNonExpiredConfirmationsByMessageIDForUser(t *testing.T) {
	t.Parallel()
	s := openMigratedStoreInternal(t)
	ctx := context.Background()

	t.Run("user_scope_rejected", func(t *testing.T) {
		sq := userSQ(s)
		_, err := sq.CountNonExpiredConfirmationsByMessageIDForUser(ctx, "msg1", "user1")
		if !errors.Is(err, errUserScopeForSystemMethod) {
			t.Errorf("user scope: want errUserScopeForSystemMethod; got: %v", err)
		}
	})

	t.Run("system_scope_passes_gate", func(t *testing.T) {
		sq := sysSQInternal(s)
		// Non-existent message → 0 count, no error (query succeeds, returns 0).
		n, err := sq.CountNonExpiredConfirmationsByMessageIDForUser(ctx, "msg-nonexistent", "user-none")
		if errors.Is(err, errUserScopeForSystemMethod) {
			t.Errorf("system scope: must not return errUserScopeForSystemMethod; got: %v", err)
		}
		if err != nil {
			t.Errorf("system scope: unexpected error: %v", err)
		}
		if n != 0 {
			t.Errorf("count for nonexistent message = %d, want 0", n)
		}
	})
}

// TestSystemGate_MarkConfirmationExpiredByUserID verifies the gate on MarkConfirmationExpiredByUserID.
func TestSystemGate_MarkConfirmationExpiredByUserID(t *testing.T) {
	t.Parallel()
	s := openMigratedStoreInternal(t)
	ctx := context.Background()

	t.Run("user_scope_rejected", func(t *testing.T) {
		sq := userSQ(s)
		_, err := sq.MarkConfirmationExpiredByUserID(ctx, "call1", "user1")
		if !errors.Is(err, errUserScopeForSystemMethod) {
			t.Errorf("user scope: want errUserScopeForSystemMethod; got: %v", err)
		}
	})

	t.Run("system_scope_passes_gate", func(t *testing.T) {
		sq := sysSQInternal(s)
		// Non-existent row → 0 rows affected, no error.
		n, err := sq.MarkConfirmationExpiredByUserID(ctx, "call-nonexistent", "user-none")
		if errors.Is(err, errUserScopeForSystemMethod) {
			t.Errorf("system scope: must not return errUserScopeForSystemMethod; got: %v", err)
		}
		if err != nil {
			t.Errorf("system scope: unexpected error: %v", err)
		}
		if n != 0 {
			t.Errorf("rows affected for nonexistent call = %d, want 0", n)
		}
	})
}

// TestSystemGate_MarkMessageAbandonedByUserID verifies the gate on MarkMessageAbandonedByUserID.
func TestSystemGate_MarkMessageAbandonedByUserID(t *testing.T) {
	t.Parallel()
	s := openMigratedStoreInternal(t)
	ctx := context.Background()

	t.Run("user_scope_rejected", func(t *testing.T) {
		sq := userSQ(s)
		err := sq.MarkMessageAbandonedByUserID(ctx, "msg1", "user1")
		if !errors.Is(err, errUserScopeForSystemMethod) {
			t.Errorf("user scope: want errUserScopeForSystemMethod; got: %v", err)
		}
	})

	t.Run("system_scope_passes_gate", func(t *testing.T) {
		sq := sysSQInternal(s)
		// Non-existent row → no error (UPDATE WHERE finds nothing; not a fatal error).
		err := sq.MarkMessageAbandonedByUserID(ctx, "msg-nonexistent", "user-none")
		if errors.Is(err, errUserScopeForSystemMethod) {
			t.Errorf("system scope: must not return errUserScopeForSystemMethod; got: %v", err)
		}
		if err != nil {
			t.Errorf("system scope: unexpected error: %v", err)
		}
	})
}

// TestSystemGate_ListLapsedConfirmations verifies the gate on ListLapsedConfirmations.
func TestSystemGate_ListLapsedConfirmations(t *testing.T) {
	t.Parallel()
	s := openMigratedStoreInternal(t)
	ctx := context.Background()

	t.Run("user_scope_rejected", func(t *testing.T) {
		sq := userSQ(s)
		rows, err := sq.ListLapsedConfirmations(ctx, "2025-01-01T00:00:00Z")
		if !errors.Is(err, errUserScopeForSystemMethod) {
			t.Errorf("user scope: want errUserScopeForSystemMethod; got: %v (rows: %v)", err, rows)
		}
		if rows != nil {
			t.Errorf("user scope: want nil rows; got %v", rows)
		}
	})

	t.Run("system_scope_passes_gate", func(t *testing.T) {
		sq := sysSQInternal(s)
		// Empty DB — returns empty slice, no error.
		rows, err := sq.ListLapsedConfirmations(ctx, "2025-01-01T00:00:00Z")
		if errors.Is(err, errUserScopeForSystemMethod) {
			t.Errorf("system scope: must not return errUserScopeForSystemMethod; got: %v", err)
		}
		if err != nil {
			t.Errorf("system scope: unexpected error: %v", err)
		}
		// Empty DB should return empty (non-nil) slice or nil — both are acceptable.
		_ = rows
	})
}
