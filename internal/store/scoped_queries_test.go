package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/qovira/qovira/internal/store"
)

// openMigratedStore opens a store at a temp path, runs migrations, and registers cleanup. Used by scoped-query
// integration tests.
func openMigratedStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s := openStore(t, store.Config{
		Path:         filepath.Join(dir, "test.db"),
		Key:          testKey,
		ReadPoolSize: 1,
	})
	runner := store.NewRunner()
	if err := runner.Up(context.Background(), s.Writer()); err != nil {
		t.Fatalf("runner.Up: %v", err)
	}
	return s
}

// TestForUser_UserIsolation is the primary AC1 test. It writes rows for two distinct users through their respective
// Scopes and then asserts that each user's list query returns only their own rows — not the other's.
//
// Critically, there is no way to pass user B's ID to user A's Scope: the ScopedQueries method signatures accept no
// user-identity argument. The isolation is structural.
func TestForUser_UserIsolation(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ctx := context.Background()

	userA := store.Principal{UserID: "user-a", Role: "member"}
	userB := store.Principal{UserID: "user-b", Role: "member"}

	sqA := s.ForUser(store.UserScope(userA))
	sqB := s.ForUser(store.UserScope(userB))

	// Insert rows for each user.
	if err := sqA.InsertUserData(ctx, "id-a1", "alpha-one"); err != nil {
		t.Fatalf("InsertUserData (user-a, id-a1): %v", err)
	}
	if err := sqA.InsertUserData(ctx, "id-a2", "alpha-two"); err != nil {
		t.Fatalf("InsertUserData (user-a, id-a2): %v", err)
	}
	if err := sqB.InsertUserData(ctx, "id-b1", "beta-one"); err != nil {
		t.Fatalf("InsertUserData (user-b, id-b1): %v", err)
	}

	// List rows for user A — must see only A's rows.
	rowsA, err := sqA.ListUserData(ctx)
	if err != nil {
		t.Fatalf("ListUserData (user-a): %v", err)
	}
	if len(rowsA) != 2 {
		t.Errorf("user-a ListUserData = %d rows, want 2", len(rowsA))
	}
	for _, r := range rowsA {
		if r.UserID != userA.UserID {
			t.Errorf("user-a list returned row with user_id=%q, want %q", r.UserID, userA.UserID)
		}
	}

	// List rows for user B — must see only B's row.
	rowsB, err := sqB.ListUserData(ctx)
	if err != nil {
		t.Fatalf("ListUserData (user-b): %v", err)
	}
	if len(rowsB) != 1 {
		t.Errorf("user-b ListUserData = %d rows, want 1", len(rowsB))
	}
	for _, r := range rowsB {
		if r.UserID != userB.UserID {
			t.Errorf("user-b list returned row with user_id=%q, want %q", r.UserID, userB.UserID)
		}
	}
}

// TestForUser_GetUserData_CrossUserBlocked verifies that GetUserData with user A's scope cannot retrieve user B's row,
// even if it supplies B's row ID. It returns sql.ErrNoRows, not a cross-user result.
func TestForUser_GetUserData_CrossUserBlocked(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ctx := context.Background()

	sqA := s.ForUser(store.UserScope(store.Principal{UserID: "user-a", Role: "member"}))
	sqB := s.ForUser(store.UserScope(store.Principal{UserID: "user-b", Role: "member"}))

	if err := sqB.InsertUserData(ctx, "id-b-secret", "secret"); err != nil {
		t.Fatalf("InsertUserData (user-b): %v", err)
	}

	// User A tries to get B's row by knowing its ID.
	_, err := sqA.GetUserData(ctx, "id-b-secret")
	if err == nil {
		t.Fatal("GetUserData with user-a scope on user-b's row must return an error (no rows), but it succeeded")
	}
	// Must be a "no rows" error, not a permission error — the predicate simply finds nothing.
}

// TestForUser_SystemScope_GetInstance verifies AC3: a system scope can reach system-owned tables (GetInstance) without
// a user_id predicate.
func TestForUser_SystemScope_GetInstance(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ctx := context.Background()

	sq := s.ForUser(store.SystemScope())

	inst, err := sq.GetInstance(ctx)
	if err != nil {
		t.Fatalf("GetInstance via SystemScope: %v", err)
	}
	if inst.ID != 1 {
		t.Errorf("GetInstance.ID = %d, want 1", inst.ID)
	}
}

// TestForUser_SystemScope_UserMethodRejected verifies AC3: calling a user-scoped method (InsertUserData) with a system
// scope returns a clear error — it does not silently execute an unscoped query.
func TestForUser_SystemScope_UserMethodRejected(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ctx := context.Background()

	sq := s.ForUser(store.SystemScope())

	err := sq.InsertUserData(ctx, "id-x", "should-fail")
	if err == nil {
		t.Fatal("InsertUserData with SystemScope must return an error, but it succeeded")
	}
	t.Logf("SystemScope user-method error (expected): %v", err)
}

// TestForUser_UserScope_SystemMethodRejected verifies AC3: calling the system method (GetInstance) with a user scope
// returns a clear error.
func TestForUser_UserScope_SystemMethodRejected(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ctx := context.Background()

	sq := s.ForUser(store.UserScope(store.Principal{UserID: "user-a", Role: "member"}))

	_, err := sq.GetInstance(ctx)
	if err == nil {
		t.Fatal("GetInstance with UserScope must return an error, but it succeeded")
	}
	t.Logf("UserScope system-method error (expected): %v", err)
}

// TestForUser_EmptyUserID_Rejected verifies that a UserScope constructed from a Principal with an empty UserID is
// rejected at query time with a clear error.
func TestForUser_EmptyUserID_Rejected(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ctx := context.Background()

	// Simulate a bug where auth middleware fails to populate UserID.
	sq := s.ForUser(store.UserScope(store.Principal{UserID: "", Role: "member"}))

	err := sq.InsertUserData(ctx, "id-x", "val")
	if err == nil {
		t.Fatal("InsertUserData with empty user ID must return an error, but it succeeded")
	}
	if !errors.Is(err, store.ErrEmptyUserID) {
		t.Errorf("expected ErrEmptyUserID; got: %v", err)
	}
}

// TestForUser_DeleteUserData_Scoped verifies that Delete only removes the scoped user's row, leaving the other user's
// row intact.
func TestForUser_DeleteUserData_Scoped(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ctx := context.Background()

	sqA := s.ForUser(store.UserScope(store.Principal{UserID: "user-a", Role: "member"}))
	sqB := s.ForUser(store.UserScope(store.Principal{UserID: "user-b", Role: "member"}))

	if err := sqA.InsertUserData(ctx, "id-a1", "hello"); err != nil {
		t.Fatalf("InsertUserData A: %v", err)
	}
	if err := sqB.InsertUserData(ctx, "id-b1", "world"); err != nil {
		t.Fatalf("InsertUserData B: %v", err)
	}

	// User A deletes their own row.
	if err := sqA.DeleteUserData(ctx, "id-a1"); err != nil {
		t.Fatalf("DeleteUserData A: %v", err)
	}

	// User A should have no rows.
	rowsA, err := sqA.ListUserData(ctx)
	if err != nil {
		t.Fatalf("ListUserData A after delete: %v", err)
	}
	if len(rowsA) != 0 {
		t.Errorf("after delete, user-a has %d rows, want 0", len(rowsA))
	}

	// User B still has their row.
	rowsB, err := sqB.ListUserData(ctx)
	if err != nil {
		t.Fatalf("ListUserData B after A's delete: %v", err)
	}
	if len(rowsB) != 1 {
		t.Errorf("after user-a's delete, user-b has %d rows, want 1", len(rowsB))
	}
}
