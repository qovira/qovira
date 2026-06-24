package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/qovira/qovira/internal/store/db"
)

// ErrConfirmationExpired is the primary expiry signal from the two callers in the lazy-expiry path:
//
//   - UpdatePendingConfirmationStatusIfCurrent returns it when the row is 'pending'
//     but expires_at is already in the past (the CAS UPDATE found zero rows because
//     the time predicate failed). This tells harness.Resolve to run the expiry CAS
//     instead of the approve/deny transition.
//   - MarkConfirmationExpired returns it when its CAS UPDATE finds zero rows —
//     either the row no longer exists or it was already transitioned out of 'pending'
//     by a concurrent Resolve or sweep (exactly-once winner semantics).
//
// Exported so harness.Resolve can map it to HTTP 409 with code "confirmation_expired".
var ErrConfirmationExpired = errors.New("store: pending confirmation has expired")

// ErrConfirmationNotFound is returned by GetPendingConfirmation when the row does not exist for the bound user. Callers
// map this to HTTP 404.
var ErrConfirmationNotFound = errors.New("store: pending confirmation not found")

// ErrConfirmationAlreadyResolved is returned by UpdatePendingConfirmationStatusIfCurrent when the row's current status
// is not "pending". Callers map this to HTTP 409.
var ErrConfirmationAlreadyResolved = errors.New("store: pending confirmation already resolved")

// InsertPendingConfirmationParams holds the caller-supplied fields for inserting a pending_confirmations row. The
// user_id is taken from the bound Scope.
type InsertPendingConfirmationParams struct {
	ID             string // gateway tool call ID (ULID); API-addressable
	ConversationID string
	MessageID      string // assistant message holding the tool_calls
	ToolName       string
	Args           string // JSON arguments from the tool call
	Risk           string // risk tier string
	Status         string // "pending" on insert
	ExpiresAt      string // RFC 3339 UTC
}

// InsertPendingConfirmation inserts a pending_confirmations row scoped to the bound user. Returns the full persisted
// row (including server-generated created_at).
func (sq *ScopedQueries) InsertPendingConfirmation(ctx context.Context, p InsertPendingConfirmationParams) (db.PendingConfirmation, error) {
	if err := sq.checkUserScope(); err != nil {
		return db.PendingConfirmation{}, fmt.Errorf("InsertPendingConfirmation: %w", err)
	}
	row, err := sq.writeQ.InsertPendingConfirmation(ctx, db.InsertPendingConfirmationParams{
		ID:             p.ID,
		ConversationID: p.ConversationID,
		MessageID:      p.MessageID,
		UserID:         sq.scope.UserID(),
		ToolName:       p.ToolName,
		Args:           p.Args,
		Risk:           p.Risk,
		Status:         p.Status,
		ExpiresAt:      p.ExpiresAt,
	})
	if err != nil {
		return db.PendingConfirmation{}, fmt.Errorf("InsertPendingConfirmation: %w", err)
	}
	return row, nil
}

// GetPendingConfirmation retrieves a pending_confirmations row by its call ID, scoped to the bound user. Returns
// ErrConfirmationNotFound when no row exists.
func (sq *ScopedQueries) GetPendingConfirmation(ctx context.Context, callID string) (db.PendingConfirmation, error) {
	if err := sq.checkUserScope(); err != nil {
		return db.PendingConfirmation{}, fmt.Errorf("GetPendingConfirmation: %w", err)
	}
	row, err := sq.readQ.GetPendingConfirmation(ctx, db.GetPendingConfirmationParams{
		ID:     callID,
		UserID: sq.scope.UserID(),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.PendingConfirmation{}, fmt.Errorf("GetPendingConfirmation: %w", ErrConfirmationNotFound)
		}
		return db.PendingConfirmation{}, fmt.Errorf("GetPendingConfirmation: %w", err)
	}
	return row, nil
}

// UpdatePendingConfirmationStatusIfCurrent atomically transitions a pending row from "pending" to the given status
// (approved or denied) using a CAS UPDATE that also requires expires_at >= now (via NOT (expires_at < @now)). If the
// row is pending but already past ExpiresAt, it is not updated and ErrConfirmationExpired is returned so the caller can
// run the expiry CAS instead.
//
// Returns:
//   - nil on success (rowsAffected == 1).
//   - ErrConfirmationNotFound when no row exists for callID + userID.
//   - ErrConfirmationAlreadyResolved when the row is resolved but not expired.
//   - ErrConfirmationExpired when the row exists, is still "pending", but is past ExpiresAt.
func (sq *ScopedQueries) UpdatePendingConfirmationStatusIfCurrent(ctx context.Context, callID, status, now string) error {
	if err := sq.checkUserScope(); err != nil {
		return fmt.Errorf("UpdatePendingConfirmationStatusIfCurrent: %w", err)
	}
	rowsAffected, err := sq.writeQ.UpdatePendingConfirmationStatusIfCurrent(ctx, db.UpdatePendingConfirmationStatusIfCurrentParams{
		Status: status,
		ID:     callID,
		UserID: sq.scope.UserID(),
		Now:    now,
	})
	if err != nil {
		return fmt.Errorf("UpdatePendingConfirmationStatusIfCurrent: %w", err)
	}
	if rowsAffected == 0 {
		// The CAS found 0 rows. Three possible reasons:
		// 1. Row doesn't exist → ErrConfirmationNotFound
		// 2. Row exists but not "pending" (already resolved) → ErrConfirmationAlreadyResolved
		// 3. Row is "pending" but past expires_at → ErrConfirmationExpired
		row, getErr := sq.readQ.GetPendingConfirmation(ctx, db.GetPendingConfirmationParams{
			ID:     callID,
			UserID: sq.scope.UserID(),
		})
		if getErr != nil {
			if errors.Is(getErr, sql.ErrNoRows) {
				return fmt.Errorf("UpdatePendingConfirmationStatusIfCurrent: %w", ErrConfirmationNotFound)
			}
			return fmt.Errorf("UpdatePendingConfirmationStatusIfCurrent: %w", getErr)
		}
		if row.Status != "pending" {
			return fmt.Errorf("UpdatePendingConfirmationStatusIfCurrent: %w", ErrConfirmationAlreadyResolved)
		}
		// Row is "pending" but expired (expires_at < now). Signal the caller to run the expiry CAS.
		return fmt.Errorf("UpdatePendingConfirmationStatusIfCurrent: %w", ErrConfirmationExpired)
	}
	return nil
}

// MarkConfirmationExpired atomically transitions a pending_confirmations row from "pending" to "expired" using a CAS
// UPDATE (WHERE id=? AND user_id=? AND status='pending').
// Returns:
//   - nil when the CAS succeeded (the row is now expired).
//   - ErrConfirmationExpired when rowsAffected==0 — the row either does not exist or
//     was already resolved/expired before this call (the sweep or a concurrent Resolve won).
//
// The caller (harness.Resolve lazy-expiry path) is responsible for reading the row after a zero-rows result to
// distinguish not-found from already-resolved/already-expired.
func (sq *ScopedQueries) MarkConfirmationExpired(ctx context.Context, callID string) error {
	if err := sq.checkUserScope(); err != nil {
		return fmt.Errorf("MarkConfirmationExpired: %w", err)
	}
	n, err := sq.writeQ.MarkConfirmationExpired(ctx, db.MarkConfirmationExpiredParams{
		ID:     callID,
		UserID: sq.scope.UserID(),
	})
	if err != nil {
		return fmt.Errorf("MarkConfirmationExpired: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("MarkConfirmationExpired: %w", ErrConfirmationExpired)
	}
	return nil
}

// ListLapsedConfirmations returns all pending_confirmations rows whose expires_at is before now and whose status is
// still "pending". This is a cross-user system housekeeping query — it is intentionally unscoped and requires a system
// scope. Each returned row carries its own user_id so the caller can issue per-row, per-user operations.
//
// Requires a system scope — returns nil, errUserScopeForSystemMethod for a user scope. This is the query backing
// SweepExpiredConfirmations.
func (sq *ScopedQueries) ListLapsedConfirmations(ctx context.Context, now string) ([]db.PendingConfirmation, error) {
	if !sq.scope.IsSystem() {
		return nil, fmt.Errorf("ListLapsedConfirmations: %w", errUserScopeForSystemMethod)
	}
	rows, err := sq.readQ.ListLapsedConfirmations(ctx, now)
	if err != nil {
		return nil, fmt.Errorf("ListLapsedConfirmations: %w", err)
	}
	return rows, nil
}

// CountNonExpiredConfirmationsByMessageID returns the count of pending_confirmations rows for the given assistant
// message that are NOT in 'expired' status (i.e. those in 'pending', 'approved', or 'denied'). User-scoped.
//
// Used to gate MarkMessageAbandoned: the assistant message is abandoned only when this count is zero — meaning no
// sibling call remains pending or was resolved by the user. This prevents premature abandonment in the multi-confirm
// case where one call expires while siblings are still pending or have been approved/denied.
func (sq *ScopedQueries) CountNonExpiredConfirmationsByMessageID(ctx context.Context, messageID string) (int64, error) {
	if err := sq.checkUserScope(); err != nil {
		return 0, fmt.Errorf("CountNonExpiredConfirmationsByMessageID: %w", err)
	}
	n, err := sq.readQ.CountNonExpiredConfirmationsByMessageID(ctx, db.CountNonExpiredConfirmationsByMessageIDParams{
		MessageID: messageID,
		UserID:    sq.scope.UserID(),
	})
	if err != nil {
		return 0, fmt.Errorf("CountNonExpiredConfirmationsByMessageID: %w", err)
	}
	return n, nil
}

// CountNonExpiredConfirmationsByMessageIDForUser returns the count of pending_confirmations rows for the given
// assistant message and user that are NOT in 'expired' status. Used by the sweep path where the user_id comes from the
// lapsed row, not the bound scope. Requires a system scope — returns 0, errUserScopeForSystemMethod for a user scope.
func (sq *ScopedQueries) CountNonExpiredConfirmationsByMessageIDForUser(ctx context.Context, messageID, userID string) (int64, error) {
	if !sq.scope.IsSystem() {
		return 0, fmt.Errorf("CountNonExpiredConfirmationsByMessageIDForUser: %w", errUserScopeForSystemMethod)
	}
	n, err := sq.readQ.CountNonExpiredConfirmationsByMessageID(ctx, db.CountNonExpiredConfirmationsByMessageIDParams{
		MessageID: messageID,
		UserID:    userID,
	})
	if err != nil {
		return 0, fmt.Errorf("CountNonExpiredConfirmationsByMessageIDForUser: %w", err)
	}
	return n, nil
}

// MarkConfirmationExpiredByUserID atomically transitions a pending row from "pending" to "expired" keyed by
// (id, user_id). Used by the sweep path where the user_id comes from each lapsed row returned by
// ListLapsedConfirmations, not the bound scope. Requires a system scope — returns 0, errUserScopeForSystemMethod for a
// user scope.
func (sq *ScopedQueries) MarkConfirmationExpiredByUserID(ctx context.Context, callID, userID string) (int64, error) {
	if !sq.scope.IsSystem() {
		return 0, fmt.Errorf("MarkConfirmationExpiredByUserID: %w", errUserScopeForSystemMethod)
	}
	return sq.writeQ.MarkConfirmationExpired(ctx, db.MarkConfirmationExpiredParams{
		ID:     callID,
		UserID: userID,
	})
}

// MarkMessageAbandonedByUserID marks a message as abandoned keyed by (id, user_id). Used by the sweep path where the
// user_id comes from each lapsed row, not the bound scope. Requires a system scope — returns
// errUserScopeForSystemMethod for a user scope.
func (sq *ScopedQueries) MarkMessageAbandonedByUserID(ctx context.Context, messageID, userID string) error {
	if !sq.scope.IsSystem() {
		return fmt.Errorf("MarkMessageAbandonedByUserID: %w", errUserScopeForSystemMethod)
	}
	_, err := sq.writeQ.MarkMessageAbandoned(ctx, db.MarkMessageAbandonedParams{
		ID:     messageID,
		UserID: userID,
	})
	if err != nil {
		return fmt.Errorf("MarkMessageAbandonedByUserID: %w", err)
	}
	return nil
}
