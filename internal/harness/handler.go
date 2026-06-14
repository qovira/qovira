package harness

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/qovira/qovira/internal/httpx"
	"github.com/qovira/qovira/internal/store"
)

// Routes registers the harness HTTP endpoints on the provided router.
//
//   - POST /api/v1/conversations/{id}/messages — persist the user message and
//     kick off the async turn, returning 202 with the persisted message.
func (h *Harness) Routes(r interface {
	HandleFunc(pattern string, handler http.HandlerFunc)
}) {
	r.HandleFunc("POST /api/v1/conversations/{id}/messages", h.handlePostMessage)
}

// handlePostMessage is the POST /api/v1/conversations/{id}/messages handler.
//
// It:
//  1. Resolves the authenticated Principal from context.
//  2. Parses the request body for {content}.
//  3. Upserts the conversation (create if new, no-op if existing).
//  4. Persists the user message.
//  5. Calls StartTurn to launch the async AI turn.
//  6. Returns 202 with the persisted message JSON body.
func (h *Harness) handlePostMessage(w http.ResponseWriter, r *http.Request) {
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

	var body InboundMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.WriteProblem(w, r, httpx.MalformedBodyProblem())
		return
	}

	scope := store.UserScope(principal)
	sq := h.store.ForUser(scope)

	// Upsert the conversation: create it if this is the first message, bump
	// updated_at if it already belongs to this user, or reject (404) if the id
	// is owned by a different user.
	if err := sq.UpsertConversation(r.Context(), convID); err != nil {
		if errors.Is(err, store.ErrConversationNotOwned) {
			// Return 404 so the response does not reveal that another user's
			// conversation exists at this id.
			httpx.WriteProblem(w, r, httpx.Problem{
				Title:  "Conversation not found",
				Status: http.StatusNotFound,
				Detail: "The requested conversation does not exist.",
				Code:   "conversation_not_found",
			})
			return
		}
		httpx.WriteProblem(w, r, httpx.InternalProblem(h.logger, "upsert_conversation_failed", err.Error()))
		return
	}

	// Persist the user message.
	msgID := generateID()
	persisted, err := sq.InsertMessage(r.Context(), store.InsertMessageParams{
		ID:             msgID,
		ConversationID: convID,
		Role:           "user",
		Content:        body.Content,
	})
	if err != nil {
		httpx.WriteProblem(w, r, httpx.InternalProblem(h.logger, "persist_message_failed", err.Error()))
		return
	}

	// Resolve origin trust.
	origin := ResolveOrigin()

	// Launch the async turn. StartTurn is guaranteed to return before the turn completes.
	if err := h.StartTurn(r.Context(), convID, body, origin, principal); err != nil {
		// StartTurn only fails if it cannot dispatch the goroutine, which is extremely rare.
		httpx.WriteProblem(w, r, httpx.InternalProblem(h.logger, "start_turn_failed", err.Error()))
		return
	}

	// Return 202 with the persisted user message.
	resp := MessageResponse{
		ID:             persisted.ID,
		ConversationID: persisted.ConversationID,
		Role:           persisted.Role,
		Content:        persisted.Content,
		CreatedAt:      persisted.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Error("harness: encode response", "err", err)
	}
}
