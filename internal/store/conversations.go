package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/qovira/qovira/internal/store/db"
)

// ErrConversationNotOwned is returned by UpsertConversation when the requested conversation id already exists and is
// owned by a different user. Callers should treat this as "not found" (404) to avoid leaking the existence of another
// user's conversation.
var ErrConversationNotOwned = errors.New("store: conversation id is owned by a different user")

// UpsertConversation ensures that a conversation with the given id exists and is owned by the bound user. Three
// outcomes are possible:
//
//   - New id: the conversation is created and owned by the bound user.
//   - Existing id, same owner: updated_at is bumped; returns nil.
//   - Existing id, different owner: returns ErrConversationNotOwned so the caller
//     can respond with 404 (not exposing that another user's conversation exists).
//
// The ownership check is performed immediately after the INSERT-if-new step. Because the write pool is capped at one
// connection all writes are serialised, eliminating TOCTOU races between the INSERT and the ownership read.
func (sq *ScopedQueries) UpsertConversation(ctx context.Context, id string) error {
	if err := sq.checkUserScope(); err != nil {
		return fmt.Errorf("UpsertConversation: %w", err)
	}

	// Attempt to insert. ON CONFLICT(id) DO NOTHING means this is a no-op when the id already exists, regardless of who
	// owns it.
	if err := sq.writeQ.UpsertConversation(ctx, db.UpsertConversationParams{
		ID:     id,
		UserID: sq.scope.UserID(),
	}); err != nil {
		return fmt.Errorf("UpsertConversation: %w", err)
	}

	// Verify ownership: GetConversation is user-scoped (WHERE id=? AND user_id=?). If the INSERT no-opped because the
	// id belongs to another user, this returns sql.ErrNoRows — mapped to ErrConversationNotOwned below.
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

	// The conversation exists and belongs to this user. Bump updated_at so that re-posting reflects the most recent
	// activity time.
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

// ListConversationsParams holds the caller-supplied parameters for listing conversations. The user_id is taken from
// the bound Scope.
type ListConversationsParams struct {
	// CursorUpdatedAt and CursorID are the keyset cursor values. When non-empty, the query skips rows that sort at or
	// before this position in (updated_at DESC, id DESC).
	CursorUpdatedAt string
	CursorID        string
	// Limit is the page size. Callers should fetch limit+1 to detect hasMore.
	Limit int64
}

// ListConversations returns a cursor-paginated slice of conversations for the bound user, ordered by (updated_at DESC,
// id DESC) — most-recently-active first. Each row includes a preview derived from the first user message in the
// conversation.
func (sq *ScopedQueries) ListConversations(ctx context.Context, p ListConversationsParams) ([]db.ListConversationsRow, error) {
	if err := sq.checkUserScope(); err != nil {
		return nil, fmt.Errorf("ListConversations: %w", err)
	}
	var cursorUpdatedAt any
	var cursorID sql.NullString
	if p.CursorUpdatedAt != "" {
		cursorUpdatedAt = p.CursorUpdatedAt
		cursorID = sql.NullString{String: p.CursorID, Valid: true}
	}
	return sq.readQ.ListConversations(ctx, db.ListConversationsParams{
		UserID:          sq.scope.UserID(),
		CursorUpdatedAt: cursorUpdatedAt,
		CursorID:        cursorID,
		Limit:           p.Limit,
	})
}

// InsertMessage persists a message row into the messages table, scoped to the bound user. It returns the full
// persisted row (including the server-generated created_at timestamp and abandoned flag). Returns an error if the
// scope is invalid.
func (sq *ScopedQueries) InsertMessage(ctx context.Context, p InsertMessageParams) (db.InsertMessageRow, error) {
	if err := sq.checkUserScope(); err != nil {
		return db.InsertMessageRow{}, fmt.Errorf("InsertMessage: %w", err)
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
// error if the scope is invalid. Each row includes the abandoned flag (non-zero means the assistant message was
// abandoned due to confirmation expiry — such messages are inert and must not be treated as outstanding work).
func (sq *ScopedQueries) ListMessages(ctx context.Context, conversationID string) ([]db.ListMessagesRow, error) {
	if err := sq.checkUserScope(); err != nil {
		return nil, fmt.Errorf("ListMessages: %w", err)
	}
	return sq.readQ.ListMessages(ctx, db.ListMessagesParams{
		ConversationID: conversationID,
		UserID:         sq.scope.UserID(),
	})
}

// InsertMessageByUserID inserts a tool-result message keyed by (conversation_id, user_id, tool_call_id) using the
// supplied userID directly rather than the bound scope. The msgID must be supplied by the caller (use id.New()). Used
// by the sweep path where the user_id comes from the lapsed row, not the bound scope. Requires a system scope —
// returns errUserScopeForSystemMethod for a user scope.
func (sq *ScopedQueries) InsertMessageByUserID(ctx context.Context, msgID, conv, userID, callID, content string) error {
	if !sq.scope.IsSystem() {
		return fmt.Errorf("InsertMessageByUserID: %w", errUserScopeForSystemMethod)
	}
	_, err := sq.writeQ.InsertMessage(ctx, db.InsertMessageParams{
		ID:             msgID,
		ConversationID: conv,
		UserID:         userID,
		Role:           "tool",
		Content:        content,
		ToolCallID:     sql.NullString{String: callID, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("InsertMessageByUserID: %w", err)
	}
	return nil
}

// MarkMessageAbandoned sets the abandoned flag on a message row to 1, scoped to the bound user. Used when a
// confirmation expires: the assistant message holding the dangling tool_calls is marked abandoned so the conversation
// is never treated as resumable for those calls.
func (sq *ScopedQueries) MarkMessageAbandoned(ctx context.Context, messageID string) error {
	if err := sq.checkUserScope(); err != nil {
		return fmt.Errorf("MarkMessageAbandoned: %w", err)
	}
	_, err := sq.writeQ.MarkMessageAbandoned(ctx, db.MarkMessageAbandonedParams{
		ID:     messageID,
		UserID: sq.scope.UserID(),
	})
	if err != nil {
		return fmt.Errorf("MarkMessageAbandoned: %w", err)
	}
	return nil
}
