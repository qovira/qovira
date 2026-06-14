package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/qovira/qovira/internal/store/db"
)

// ErrConversationNotOwned is returned by UpsertConversation when the requested
// conversation id already exists and is owned by a different user. Callers
// should treat this as "not found" (404) to avoid leaking the existence of
// another user's conversation.
var ErrConversationNotOwned = errors.New("store: conversation id is owned by a different user")

// UpsertConversation ensures that a conversation with the given id exists and is
// owned by the bound user. Three outcomes are possible:
//
//   - New id: the conversation is created and owned by the bound user.
//   - Existing id, same owner: updated_at is bumped; returns nil.
//   - Existing id, different owner: returns ErrConversationNotOwned so the caller
//     can respond with 404 (not exposing that another user's conversation exists).
//
// The ownership check is performed immediately after the INSERT-if-new step.
// Because the write pool is capped at one connection all writes are serialised,
// eliminating TOCTOU races between the INSERT and the ownership read.
func (sq *ScopedQueries) UpsertConversation(ctx context.Context, id string) error {
	if err := sq.checkUserScope(); err != nil {
		return fmt.Errorf("UpsertConversation: %w", err)
	}

	// Attempt to insert. ON CONFLICT(id) DO NOTHING means this is a no-op when the
	// id already exists, regardless of who owns it.
	if err := sq.writeQ.UpsertConversation(ctx, db.UpsertConversationParams{
		ID:     id,
		UserID: sq.scope.UserID(),
	}); err != nil {
		return fmt.Errorf("UpsertConversation: %w", err)
	}

	// Verify ownership: GetConversation is user-scoped (WHERE id=? AND user_id=?).
	// If the INSERT no-opped because the id belongs to another user, this returns
	// sql.ErrNoRows — mapped to ErrConversationNotOwned below.
	_, err := sq.readQ.GetConversation(ctx, db.GetConversationParams{
		ID:     id,
		UserID: sq.scope.UserID(),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("UpsertConversation: %w", ErrConversationNotOwned)
		}
		return fmt.Errorf("UpsertConversation: verify ownership: %w", err)
	}

	// The conversation exists and belongs to this user. Bump updated_at so that
	// re-posting reflects the most recent activity time.
	if err := sq.writeQ.TouchConversation(ctx, db.TouchConversationParams{
		ID:     id,
		UserID: sq.scope.UserID(),
	}); err != nil {
		return fmt.Errorf("UpsertConversation: touch: %w", err)
	}

	return nil
}

// GetConversation retrieves a conversation by id, scoped to the bound user. Returns sql.ErrNoRows (via the db layer)
// when no matching row exists.
func (sq *ScopedQueries) GetConversation(ctx context.Context, id string) (db.Conversation, error) {
	if err := sq.checkUserScope(); err != nil {
		return db.Conversation{}, fmt.Errorf("GetConversation: %w", err)
	}
	return sq.readQ.GetConversation(ctx, db.GetConversationParams{
		ID:     id,
		UserID: sq.scope.UserID(),
	})
}

// InsertMessage persists a message row into the messages table, scoped to the bound user. It returns the full persisted
// row (including the server-generated created_at timestamp). Returns an error if the scope is invalid.
func (sq *ScopedQueries) InsertMessage(ctx context.Context, p InsertMessageParams) (db.Message, error) {
	if err := sq.checkUserScope(); err != nil {
		return db.Message{}, fmt.Errorf("InsertMessage: %w", err)
	}
	return sq.writeQ.InsertMessage(ctx, db.InsertMessageParams{
		ID:             p.ID,
		ConversationID: p.ConversationID,
		UserID:         sq.scope.UserID(),
		Role:           p.Role,
		Content:        p.Content,
		ToolCalls:      sql.NullString{String: p.ToolCalls, Valid: p.ToolCalls != ""},
		ToolCallID:     sql.NullString{String: p.ToolCallID, Valid: p.ToolCallID != ""},
		FinishReason:   sql.NullString{String: p.FinishReason, Valid: p.FinishReason != ""},
	})
}

// InsertMessageParams holds the caller-supplied fields for inserting a message row. The user_id is taken from the bound
// Scope; the caller must NOT pass it.
type InsertMessageParams struct {
	ID             string
	ConversationID string
	Role           string
	Content        string
	ToolCalls      string // JSON array string or "" for NULL
	ToolCallID     string // "" for NULL
	FinishReason   string // "" for NULL
}

// ListMessages returns all messages for a conversation, ordered by created_at, scoped to the bound user. Returns an
// error if the scope is invalid.
func (sq *ScopedQueries) ListMessages(ctx context.Context, conversationID string) ([]db.Message, error) {
	if err := sq.checkUserScope(); err != nil {
		return nil, fmt.Errorf("ListMessages: %w", err)
	}
	return sq.readQ.ListMessages(ctx, db.ListMessagesParams{
		ConversationID: conversationID,
		UserID:         sq.scope.UserID(),
	})
}
