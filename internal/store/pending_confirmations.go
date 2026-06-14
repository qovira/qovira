package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/qovira/qovira/internal/store/db"
)

// ErrConfirmationNotFound is returned by GetPendingConfirmation when the row
// does not exist for the bound user. Callers map this to HTTP 404.
var ErrConfirmationNotFound = errors.New("store: pending confirmation not found")

// ErrConfirmationAlreadyResolved is returned by UpdatePendingConfirmationStatus
// when the row's current status is not "pending". Callers map this to HTTP 409.
var ErrConfirmationAlreadyResolved = errors.New("store: pending confirmation already resolved")

// InsertPendingConfirmationParams holds the caller-supplied fields for inserting
// a pending_confirmations row. The user_id is taken from the bound Scope.
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

// InsertPendingConfirmation inserts a pending_confirmations row scoped to the bound user.
// Returns the full persisted row (including server-generated created_at).
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

// GetPendingConfirmation retrieves a pending_confirmations row by its call ID,
// scoped to the bound user. Returns ErrConfirmationNotFound when no row exists.
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

// UpdatePendingConfirmationStatus atomically transitions a pending_confirmations
// row from "pending" to the given status (approved or denied) using a
// compare-and-swap UPDATE (WHERE status='pending'). It returns:
//   - nil on success (rowsAffected == 1).
//   - ErrConfirmationNotFound when no row exists for callID + userID.
//   - ErrConfirmationAlreadyResolved when the row exists but is not "pending"
//     (rowsAffected == 0 after a successful UPDATE with status predicate).
//
// The CAS eliminates the non-atomic read-then-write from the previous
// implementation, making concurrent Resolve calls on the same callID safe:
// exactly one caller wins the UPDATE and proceeds; the other sees rowsAffected=0
// and returns ErrConfirmationAlreadyResolved → HTTP 409.
func (sq *ScopedQueries) UpdatePendingConfirmationStatus(ctx context.Context, callID, status string) error {
	if err := sq.checkUserScope(); err != nil {
		return fmt.Errorf("UpdatePendingConfirmationStatus: %w", err)
	}
	rowsAffected, err := sq.writeQ.UpdatePendingConfirmationStatus(ctx, db.UpdatePendingConfirmationStatusParams{
		Status: status,
		ID:     callID,
		UserID: sq.scope.UserID(),
	})
	if err != nil {
		return fmt.Errorf("UpdatePendingConfirmationStatus: %w", err)
	}
	if rowsAffected == 0 {
		// Either the row does not exist or it is already resolved. Distinguish by
		// looking it up (read-only; we only reach here on the rare contention path).
		row, getErr := sq.readQ.GetPendingConfirmation(ctx, db.GetPendingConfirmationParams{
			ID:     callID,
			UserID: sq.scope.UserID(),
		})
		if getErr != nil {
			if errors.Is(getErr, sql.ErrNoRows) {
				return fmt.Errorf("UpdatePendingConfirmationStatus: %w", ErrConfirmationNotFound)
			}
			return fmt.Errorf("UpdatePendingConfirmationStatus: %w", getErr)
		}
		// Row exists but was not "pending" — another Resolve won the CAS race.
		_ = row
		return fmt.Errorf("UpdatePendingConfirmationStatus: %w", ErrConfirmationAlreadyResolved)
	}
	return nil
}

// ListPendingConfirmationsByConversation returns all pending_confirmations rows
// for a conversation, ordered by (created_at, id), scoped to the bound user.
func (sq *ScopedQueries) ListPendingConfirmationsByConversation(ctx context.Context, conversationID string) ([]db.PendingConfirmation, error) {
	if err := sq.checkUserScope(); err != nil {
		return nil, fmt.Errorf("ListPendingConfirmationsByConversation: %w", err)
	}
	rows, err := sq.readQ.ListPendingConfirmationsByConversation(ctx, db.ListPendingConfirmationsByConversationParams{
		ConversationID: conversationID,
		UserID:         sq.scope.UserID(),
	})
	if err != nil {
		return nil, fmt.Errorf("ListPendingConfirmationsByConversation: %w", err)
	}
	return rows, nil
}
