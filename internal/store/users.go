package store

import (
	"context"
	"fmt"
)

// HasAnyUser reports whether at least one row exists in the users table. It returns (true, nil) when at least one user
// account exists, (false, nil) when the table is empty, and (false, err) on a store failure.
//
// users is system-owned (no user_id column — it is the identity table from which per-user scope is derived). This query
// is therefore a system-scoped read that needs no user_id predicate; it goes through the read pool.
//
// HasAnyUser satisfies the bootstrap.UserExister seam, allowing the composition root to pass the *Store directly
// without an adapter.
func (s *Store) HasAnyUser(ctx context.Context) (bool, error) {
	var has bool
	err := s.readDB.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM users)",
	).Scan(&has)
	if err != nil {
		return false, fmt.Errorf("store: has any user: %w", err)
	}
	return has, nil
}
