package harness

// conversations.go — read-only conversation endpoints.
//
//   - GET /api/v1/conversations        — paginated list (most-recently-active first)
//   - GET /api/v1/conversations/{id}   — full message history

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/qovira/qovira/internal/httpx"
	"github.com/qovira/qovira/internal/store"
	"github.com/qovira/qovira/internal/store/db"
)

// ── Response types ────────────────────────────────────────────────────────────

// ConversationListItem is one item in the GET /api/v1/conversations list.
type ConversationListItem struct {
	ID        string `json:"id"`
	Preview   string `json:"preview"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// ConversationDetail is the body returned by GET /api/v1/conversations/{id}.
type ConversationDetail struct {
	ID        string           `json:"id"`
	CreatedAt string           `json:"createdAt"`
	UpdatedAt string           `json:"updatedAt"`
	Messages  []HistoryMessage `json:"messages"`
}

// HistoryMessage is one message in the conversation history response. Optional fields (ToolCalls, ToolCallID,
// FinishReason) are omitted when absent.
type HistoryMessage struct {
	ID           string          `json:"id"`
	Role         string          `json:"role"`
	Content      string          `json:"content"`
	CreatedAt    string          `json:"createdAt"`
	ToolCalls    json.RawMessage `json:"toolCalls,omitempty"`
	ToolCallID   string          `json:"toolCallId,omitempty"`
	FinishReason string          `json:"finishReason,omitempty"`
	Abandoned    bool            `json:"abandoned"`
}

// ── Cursor helpers ────────────────────────────────────────────────────────────

// convListCursor is the internal structure encoded into the opaque pagination cursor for the conversations list. The
// keyset is (UpdatedAt DESC, ID DESC).
type convListCursor struct {
	UpdatedAt string `json:"u"` // RFC 3339 UTC
	ID        string `json:"i"`
}

// encodeConvCursor base64-encodes a JSON convListCursor.
func encodeConvCursor(updatedAt, id string) string {
	raw, _ := json.Marshal(convListCursor{UpdatedAt: updatedAt, ID: id})
	return base64.RawStdEncoding.EncodeToString(raw)
}

// decodeConvCursor reverses encodeConvCursor. Returns an error when the input is not valid base64 or its JSON is
// malformed.
func decodeConvCursor(cursor string) (updatedAt, id string, err error) {
	raw, err := base64.RawStdEncoding.DecodeString(cursor)
	if err != nil {
		return "", "", fmt.Errorf("conversations: decode cursor: %w", err)
	}
	var c convListCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", "", fmt.Errorf("conversations: decode cursor: invalid json: %w", err)
	}
	if c.UpdatedAt == "" || c.ID == "" {
		return "", "", fmt.Errorf("conversations: decode cursor: missing required fields")
	}
	return c.UpdatedAt, c.ID, nil
}

// ── Preview truncation ────────────────────────────────────────────────────────

// maxPreviewRunes is the maximum number of Unicode code points in a preview before it is truncated with "...".
const maxPreviewRunes = 80

// previewString coerces the generated preview column (typed interface{} because it comes from a COALESCE expression)
// to a string. The query coalesces a missing preview to an empty string, guaranteeing a non-NULL value; in practice
// the SQLCipher driver returns a string, but a driver returning []byte for a TEXT column is also handled. Any other
// shape (or nil) yields an empty string rather than panicking — a missing preview, never a crash.
func previewString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	default:
		return ""
	}
}

// truncatePreview returns content truncated to maxPreviewRunes runes with "..." appended when truncation occurs.
// Returns the original string when short enough.
func truncatePreview(content string) string {
	if utf8.RuneCountInString(content) <= maxPreviewRunes {
		return content
	}
	runes := []rune(content)
	return string(runes[:maxPreviewRunes]) + "..."
}

// ── List known params ─────────────────────────────────────────────────────────

// listConvKnownParams is the set of accepted query parameter names for GET /api/v1/conversations. Unknown names are
// rejected with 400 unknown_query_param.
var listConvKnownParams = map[string]struct{}{
	"cursor": {},
	"limit":  {},
}

const (
	convListDefaultLimit = 25
	convListMaxLimit     = 100
)

// ── Handlers ──────────────────────────────────────────────────────────────────

// handleListConversations handles GET /api/v1/conversations.
//
// Query parameters (all optional):
//   - cursor  opaque pagination cursor from a prior page's nextCursor
//   - limit   page size (default 25, max 100)
//
// Returns: 200 { data: [...ConversationListItem], pagination: { nextCursor, hasMore } }
func (h *Harness) handleListConversations(w http.ResponseWriter, r *http.Request) {
	principal, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, httpx.Problem{
			Title:  "Authentication required",
			Status: http.StatusUnauthorized,
			Detail: "No authenticated principal found.",
			Code:   "unauthenticated",
		})
		return
	}

	q := r.URL.Query()

	// Reject unknown query parameters.
	for name := range q {
		if _, known := listConvKnownParams[name]; !known {
			httpx.WriteProblem(w, r, httpx.Problem{
				Title:  "Unknown query parameter",
				Status: http.StatusBadRequest,
				Detail: fmt.Sprintf("Query parameter %q is not recognised. Accepted parameters: cursor, limit.", name),
				Code:   "unknown_query_param",
			})
			return
		}
	}

	// Parse limit.
	limit := convListDefaultLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			httpx.WriteProblem(w, r, httpx.ValidationProblem(
				"validation_error",
				"Request validation failed.",
				httpx.FieldError{Pointer: "/limit", Detail: "limit must be a positive integer"},
			))
			return
		}
		limit = n
	}
	if limit > convListMaxLimit {
		limit = convListMaxLimit
	}

	// Decode cursor.
	var cursorUpdatedAt, cursorID string
	if raw := q.Get("cursor"); raw != "" {
		var err error
		cursorUpdatedAt, cursorID, err = decodeConvCursor(raw)
		if err != nil {
			httpx.WriteProblem(w, r, httpx.Problem{
				Title:  "Invalid cursor",
				Status: http.StatusBadRequest,
				Detail: "The cursor value is malformed or has been corrupted.",
				Code:   "invalid_cursor",
			})
			return
		}
	}

	scope := store.UserScope(principal)
	sq := h.store.ForUser(scope)

	// Fetch limit+1 rows to detect hasMore.
	rows, err := sq.ListConversations(r.Context(), store.ListConversationsParams{
		CursorUpdatedAt: cursorUpdatedAt,
		CursorID:        cursorID,
		Limit:           int64(limit + 1),
	})
	if err != nil {
		httpx.WriteProblem(w, r, httpx.InternalProblem(h.logger, "list_conversations_failed", err.Error()))
		return
	}

	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}

	items := make([]ConversationListItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, ConversationListItem{
			ID:        row.ID,
			Preview:   truncatePreview(previewString(row.Preview)),
			CreatedAt: row.CreatedAt,
			UpdatedAt: row.UpdatedAt,
		})
	}

	var nextCursor *string
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		enc := encodeConvCursor(last.UpdatedAt, last.ID)
		nextCursor = &enc
	}

	envelope := httpx.Page[ConversationListItem]{
		Data: items,
		Pagination: httpx.PagePagination{
			NextCursor: nextCursor,
			HasMore:    hasMore,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(envelope); err != nil {
		h.logger.Error("harness: encode list conversations response", "err", err)
	}
}

// handleGetConversation handles GET /api/v1/conversations/{id}.
//
// Returns the full chronological message history for the conversation. 404 when the conversation doesn't exist or
// belongs to another user.
func (h *Harness) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	principal, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, httpx.Problem{
			Title:  "Authentication required",
			Status: http.StatusUnauthorized,
			Detail: "No authenticated principal found.",
			Code:   "unauthenticated",
		})
		return
	}

	convID := r.PathValue("id")
	if convID == "" {
		httpx.WriteProblem(w, r, httpx.ValidationProblem(
			"missing_conversation_id",
			"The conversation ID path parameter is required.",
		))
		return
	}

	scope := store.UserScope(principal)
	sq := h.store.ForUser(scope)

	// Verify ownership via GetConversation (returns sql.ErrNoRows → 404).
	conv, err := sq.GetConversation(r.Context(), convID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.WriteProblem(w, r, httpx.Problem{
				Title:  "Conversation not found",
				Status: http.StatusNotFound,
				Detail: "The requested conversation does not exist.",
				Code:   "conversation_not_found",
			})
			return
		}
		httpx.WriteProblem(w, r, httpx.InternalProblem(h.logger, "get_conversation_failed", err.Error()))
		return
	}

	// Load messages in chronological order.
	msgs, err := sq.ListMessages(r.Context(), convID)
	if err != nil {
		httpx.WriteProblem(w, r, httpx.InternalProblem(h.logger, "list_messages_failed", err.Error()))
		return
	}

	history := make([]HistoryMessage, 0, len(msgs))
	for _, m := range msgs {
		history = append(history, historyMessageFromRow(m))
	}

	resp := ConversationDetail{
		ID:        conv.ID,
		CreatedAt: conv.CreatedAt,
		UpdatedAt: conv.UpdatedAt,
		Messages:  history,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Error("harness: encode get conversation response", "err", err)
	}
}

// historyMessageFromRow converts a db.ListMessagesRow to a HistoryMessage. Nullable fields (ToolCalls, ToolCallID,
// FinishReason) are omitted when absent. Tool message content is JSON — passed through verbatim as a string (not
// re-encoded).
func historyMessageFromRow(m db.ListMessagesRow) HistoryMessage {
	msg := HistoryMessage{
		ID:        m.ID,
		Role:      m.Role,
		Content:   m.Content,
		CreatedAt: m.CreatedAt,
		Abandoned: m.Abandoned != 0,
	}
	if m.ToolCalls.Valid && m.ToolCalls.String != "" {
		msg.ToolCalls = json.RawMessage(m.ToolCalls.String)
	}
	if m.ToolCallID.Valid && m.ToolCallID.String != "" {
		msg.ToolCallID = m.ToolCallID.String
	}
	if m.FinishReason.Valid && m.FinishReason.String != "" {
		msg.FinishReason = m.FinishReason.String
	}
	return msg
}
