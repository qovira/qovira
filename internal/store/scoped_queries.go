package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/qovira/qovira/internal/store/db"
)

// ScopedQueries wraps the sqlc-generated *db.Queries and a bound Scope so
// that every data-access method automatically filters by the scope's user ID.
// Callers obtain a *ScopedQueries via Store.ForUser; they cannot supply an
// arbitrary user ID to any method — the ID comes only from the bound Scope.
type ScopedQueries struct {
	scope Scope
	// readQ is backed by the store's read pool for SELECT-only methods.
	readQ *db.Queries
	// writeQ is backed by the store's write pool for INSERT/UPDATE/DELETE.
	writeQ *db.Queries
}

// ForUser binds scope to the store's query pools and returns a *ScopedQueries.
// Both user and system scopes are accepted; the individual methods on
// *ScopedQueries enforce scope-type rules at call time.
func (s *Store) ForUser(scope Scope) *ScopedQueries {
	return &ScopedQueries{
		scope:  scope,
		readQ:  db.New(s.readDB),
		writeQ: db.New(s.writeDB),
	}
}

// checkUserScope validates that the bound scope is a non-system scope with a
// non-empty user ID. It is called at the top of every user-scoped method.
func (sq *ScopedQueries) checkUserScope() error {
	if sq.scope.IsSystem() {
		return ErrSystemScope
	}
	if sq.scope.UserID() == "" {
		return ErrEmptyUserID
	}
	return nil
}

// InsertUserData inserts a row into the user_data exemplar table, scoped to
// the bound user. The user_id is taken from the Scope — the caller must NOT
// pass it. Returns an error if the scope is a system scope or has an empty
// user ID.
func (sq *ScopedQueries) InsertUserData(ctx context.Context, id, value string) error {
	if err := sq.checkUserScope(); err != nil {
		return fmt.Errorf("InsertUserData: %w", err)
	}
	return sq.writeQ.InsertUserData(ctx, db.InsertUserDataParams{
		ID:     id,
		UserID: sq.scope.UserID(),
		Value:  value,
	})
}

// GetUserData retrieves a single user_data row by id, scoped to the bound
// user. Returns an error if the scope is invalid.
func (sq *ScopedQueries) GetUserData(ctx context.Context, id string) (db.UserDatum, error) {
	if err := sq.checkUserScope(); err != nil {
		return db.UserDatum{}, fmt.Errorf("GetUserData: %w", err)
	}
	return sq.readQ.GetUserData(ctx, db.GetUserDataParams{
		ID:     id,
		UserID: sq.scope.UserID(),
	})
}

// ListUserData returns all user_data rows for the bound user, ordered by
// created_at. Returns an error if the scope is invalid.
func (sq *ScopedQueries) ListUserData(ctx context.Context) ([]db.UserDatum, error) {
	if err := sq.checkUserScope(); err != nil {
		return nil, fmt.Errorf("ListUserData: %w", err)
	}
	return sq.readQ.ListUserData(ctx, sq.scope.UserID())
}

// DeleteUserData deletes a user_data row by id, scoped to the bound user.
// Returns an error if the scope is invalid.
func (sq *ScopedQueries) DeleteUserData(ctx context.Context, id string) error {
	if err := sq.checkUserScope(); err != nil {
		return fmt.Errorf("DeleteUserData: %w", err)
	}
	return sq.writeQ.DeleteUserData(ctx, db.DeleteUserDataParams{
		ID:     id,
		UserID: sq.scope.UserID(),
	})
}

// GetInstance retrieves the system instance row. It is a system-scoped
// operation — available only when the Scope is a SystemScope.
//
// Calling GetInstance with a user scope returns an error to prevent mistakenly
// routing authenticated-user requests through the system path.
func (sq *ScopedQueries) GetInstance(ctx context.Context) (db.Instance, error) {
	if !sq.scope.IsSystem() {
		return db.Instance{}, fmt.Errorf("GetInstance: %w",
			errUserScopeForSystemMethod)
	}
	return sq.readQ.GetInstance(ctx)
}

// errUserScopeForSystemMethod is returned when a system-only method is called
// with a user scope.
var errUserScopeForSystemMethod = errors.New("store: system-only method called with a user scope")
