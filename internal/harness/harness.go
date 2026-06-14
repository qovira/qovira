// Package harness is the AI turn orchestrator. It wires the capability registry,
// model gateway, store, and event bus into a single entry point: a POST request
// persists the user message, launches the turn asynchronously, and the turn
// streams the assistant reply over the event bus.
//
// For this first slice, only text-only replies are supported; tool calls and
// multi-round loops are out of scope.
package harness

import (
	"context"
	"fmt"
	"iter"
	"log/slog"

	"github.com/qovira/qovira/internal/capability"
	"github.com/qovira/qovira/internal/events"
	"github.com/qovira/qovira/internal/gateway"
	"github.com/qovira/qovira/internal/store"
)

// ── Narrow interfaces ─────────────────────────────────────────────────────────

// Chatter is the narrow interface the harness uses to call the model gateway.
// *gateway.Gateway satisfies it; tests inject a fakeChatter.
type Chatter interface {
	Chat(ctx context.Context, req gateway.ChatRequest) (iter.Seq2[gateway.Chunk, error], error)
}

// Cataloger is the narrow interface the harness uses to read the capability registry.
// *capability.Registry satisfies it.
type Cataloger interface {
	Catalog() []capability.Tool
}

// Logger is a re-export alias that lets callers pass a *slog.Logger without
// importing slog directly in test helpers. The harness uses slog internally.
type Logger = slog.Logger

// NewDiscardLogger returns a Logger that discards all output below Error, for use in tests.
func NewDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(nilWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

type nilWriter struct{}

func (nilWriter) Write(p []byte) (int, error) { return len(p), nil }

// ── Domain types ──────────────────────────────────────────────────────────────

// ConversationID is the ULID string identifier of a conversation.
type ConversationID = string

// TrustLevel classifies the trust of an inbound request origin.
type TrustLevel int

const (
	// Untrusted is the zero value: the origin has not been verified.
	Untrusted TrustLevel = iota
	// Trusted indicates the origin has been verified (e.g. authenticated web session).
	Trusted
)

// Origin describes the inbound request's source and trust level.
type Origin struct {
	// Channel is the delivery channel (e.g. "web").
	Channel string
	// Trust is the resolved trust level for this origin.
	Trust TrustLevel
}

// ResolveOrigin returns the Origin for an authenticated web session.
// For this slice, all authenticated requests resolve to Trusted/"web".
func ResolveOrigin() Origin {
	return Origin{Channel: "web", Trust: Trusted}
}

// InboundMessage is the user-supplied content for a new turn.
type InboundMessage struct {
	Content string `json:"content"`
}

// MessageResponse is the JSON body returned by POST .../messages.
type MessageResponse struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversationId"`
	Role           string `json:"role"`
	Content        string `json:"content"`
	CreatedAt      string `json:"createdAt"`
}

// ── Event payload types ───────────────────────────────────────────────────────

// DeltaPayload is the Data for a "message.delta" event.
type DeltaPayload struct {
	ConversationID string `json:"conversationId"`
	Text           string `json:"text"`
}

// CompletedPayload is the Data for a "message.completed" event.
type CompletedPayload struct {
	MessageID    string `json:"messageId"`
	FinishReason string `json:"finishReason"`
}

// ── Config ────────────────────────────────────────────────────────────────────

// Config holds boot-time configuration for the harness. It is intentionally
// minimal for this first slice; additional knobs (step cap, sliding window, etc.)
// are added in later issues.
type Config struct{}

// ── Harness ───────────────────────────────────────────────────────────────────

// Harness is the AI turn orchestrator. Obtain one via New; the zero value is not valid.
type Harness struct {
	reg    Cataloger
	gw     Chatter
	store  *store.Store
	bus    events.Publisher
	cfg    Config
	logger *slog.Logger
}

// New constructs a Harness wired with the given collaborators.
//
//   - reg provides the tool catalog sent to the model.
//   - gw is the model gateway (via the narrow Chatter interface).
//   - st is the encrypted data store.
//   - bus is the event publisher (narrow Publisher interface).
//   - cfg is the harness configuration.
//   - logger is the structured logger for internal diagnostics.
func New(reg Cataloger, gw Chatter, st *store.Store, bus events.Publisher, cfg Config, logger *slog.Logger) *Harness {
	if logger == nil {
		logger = slog.Default()
	}
	return &Harness{
		reg:    reg,
		gw:     gw,
		store:  st,
		bus:    bus,
		cfg:    cfg,
		logger: logger,
	}
}

// Name satisfies the app.Module interface so the harness can be registered as a module.
func (h *Harness) Name() string { return "harness" }

// Tools returns nil — the harness does not contribute capability tools.
func (h *Harness) Tools() []capability.Tool { return nil }

// ── StartTurn ────────────────────────────────────────────────────────────────

// StartTurn launches the AI turn for the given conversation asynchronously. It
// returns once the user message from msg has been persisted and the turn goroutine
// has been dispatched. The turn itself (gateway call, streaming, persistence) runs
// in a background goroutine and its output is published over the event bus.
//
// The request context is intentionally NOT forwarded to the background goroutine:
// the turn must outlive the HTTP request (it streams to the bus, not to the
// response body), so context.Background() is used to ensure the turn is never
// cancelled when the request ends.
//
// Callers must have already persisted the user message via the HTTP handler; this
// method is responsible for launching the background turn run.
func (h *Harness) StartTurn(
	_ context.Context,
	conv ConversationID,
	_ InboundMessage,
	_ Origin,
	principal store.Principal,
) error {
	//nolint:gosec // Intentional: the turn must outlive the request context; background context is correct here.
	go func() {
		if err := h.run(context.Background(), conv, principal); err != nil {
			h.logger.Error("harness: turn failed", "conversationId", conv, "err", err)
		}
	}()
	return nil
}

// ── run ──────────────────────────────────────────────────────────────────────

// run executes a single turn: assembles the gateway request from persisted state,
// streams the reply, publishes delta events, and persists the assistant message
// before emitting message.completed.
func (h *Harness) run(ctx context.Context, conv ConversationID, principal store.Principal) error {
	scope := store.UserScope(principal)
	sq := h.store.ForUser(scope)

	// Build the gateway request from persisted message history.
	msgs, err := sq.ListMessages(ctx, conv)
	if err != nil {
		return fmt.Errorf("harness: list messages: %w", err)
	}

	// Assemble gateway messages: prepend a minimal system prompt, then history.
	const systemPrompt = "You are Qovira, a helpful personal assistant."
	gwMsgs := make([]gateway.Message, 0, len(msgs)+1)
	gwMsgs = append(gwMsgs, gateway.Message{Role: "system", Content: systemPrompt})
	for _, m := range msgs {
		gwMsgs = append(gwMsgs, gateway.Message{
			Role:    m.Role,
			Content: m.Content,
		})
	}

	// Map the capability catalog to gateway tool schemas.
	tools := h.reg.Catalog()
	gwTools := make([]gateway.ToolSchema, 0, len(tools))
	for _, t := range tools {
		gwTools = append(gwTools, gateway.ToolSchema{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Schema,
		})
	}

	req := gateway.ChatRequest{
		Messages: gwMsgs,
		Tools:    gwTools,
	}

	seq, err := h.gw.Chat(ctx, req)
	if err != nil {
		return fmt.Errorf("harness: chat setup: %w", err)
	}

	// Stream the response, accumulating text and publishing delta events.
	var textAccum []byte
	for chunk, chunkErr := range seq {
		if chunkErr != nil {
			return fmt.Errorf("harness: stream error: %w", chunkErr)
		}
		if chunk.TextDelta != "" {
			textAccum = append(textAccum, chunk.TextDelta...)
			h.bus.Publish(principal.UserID, events.Event{
				Type: "message.delta",
				Data: DeltaPayload{
					ConversationID: conv,
					Text:           chunk.TextDelta,
				},
			})
		}
		if chunk.Done {
			break
		}
	}

	// Persist the assistant message BEFORE emitting message.completed.
	assistantID, err := persistAssistantMessage(ctx, sq, conv, string(textAccum))
	if err != nil {
		return fmt.Errorf("harness: persist assistant message: %w", err)
	}

	// Emit message.completed.
	h.bus.Publish(principal.UserID, events.Event{
		Type: "message.completed",
		Data: CompletedPayload{
			MessageID:    assistantID,
			FinishReason: "stop",
		},
	})

	return nil
}

// persistAssistantMessage inserts the assistant reply into the messages table
// and returns the new message ID.
func persistAssistantMessage(ctx context.Context, sq *store.ScopedQueries, conv, content string) (string, error) {
	// Generate the ID here so we can return it without a second query.
	msgID := generateID()
	_, err := sq.InsertMessage(ctx, store.InsertMessageParams{
		ID:             msgID,
		ConversationID: conv,
		Role:           "assistant",
		Content:        content,
		FinishReason:   "stop",
	})
	if err != nil {
		return "", err
	}
	return msgID, nil
}
