// Package harness is the AI turn orchestrator. It wires the capability registry,
// model gateway, store, and event bus into a single entry point: a POST request
// persists the user message, launches the turn asynchronously, and the turn
// streams the assistant reply over the event bus.
//
// Tool calls are fully supported: the harness accumulates tool calls from the
// model stream, executes them in order through the capability registry, persists
// results, and loops back to the model until a text-only reply ends the turn or
// the configured step cap is reached.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"unicode/utf8"

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

// ToolStartedPayload is the Data for a "tool.started" event, emitted before a
// tool call is executed. Risk is rendered as its string name (e.g. "read",
// "write", "external", "destructive"). ArgsSummary is compact JSON of the call
// arguments, truncated to a reasonable length.
type ToolStartedPayload struct {
	CallID      string `json:"callId"`
	Name        string `json:"name"`
	Risk        string `json:"risk"`
	ArgsSummary string `json:"argsSummary"`
}

// ToolCompletedPayload is the Data for a "tool.completed" event, emitted after
// a tool call has been executed and its result persisted.
type ToolCompletedPayload struct {
	CallID string `json:"callId"`
	Result any    `json:"result"`
}

// ToolFailedPayload is the Data for a "tool.failed" event, emitted when a tool
// call returns a *capability.ToolError (a model-visible, self-correctable error).
// Error carries only the model-safe ToolError.Message — no internal detail.
type ToolFailedPayload struct {
	CallID string `json:"callId"`
	Error  string `json:"error"`
}

// TurnFailedPayload is the Data for a "turn.failed" event, emitted when the
// turn aborts due to an infrastructure error (auth failure, upstream error,
// network failure, recovered panic, etc.). Code is a stable, generic class
// string — NOT the raw error text; the raw detail goes to the server log only.
type TurnFailedPayload struct {
	Code string `json:"code"`
}

// ── Config ────────────────────────────────────────────────────────────────────

// defaultStepCap is the maximum number of model rounds per turn when no explicit
// cap is configured.
const defaultStepCap = 8

// Config holds boot-time configuration for the harness.
type Config struct {
	// StepCap limits the number of model-gateway rounds (including tool-call
	// loops) per turn. If <= 0, the default of 8 is applied. On reaching the
	// cap the turn ends gracefully with a final assistant message.
	StepCap int
}

// ── Harness ───────────────────────────────────────────────────────────────────

// Harness is the AI turn orchestrator. Obtain one via New; the zero value is not valid.
type Harness struct {
	reg     Cataloger
	gw      Chatter
	store   *store.Store
	bus     events.Publisher
	stepCap int
	logger  *slog.Logger
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
	stepCap := cfg.StepCap
	if stepCap <= 0 {
		stepCap = defaultStepCap
	}
	return &Harness{
		reg:     reg,
		gw:      gw,
		store:   st,
		bus:     bus,
		stepCap: stepCap,
		logger:  logger,
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
		// Panic recovery: a panic anywhere in run (including in tool Execute or
		// gateway code) is infrastructure — abort the turn cleanly so the goroutine
		// does not crash the server.
		defer func() {
			if r := recover(); r != nil {
				h.logger.Error("harness: turn panicked", "conversationId", conv, "panic", r)
				h.bus.Publish(principal.UserID, events.Event{
					Type: "turn.failed",
					Data: TurnFailedPayload{Code: "infrastructure"},
				})
			}
		}()

		if err := h.run(context.Background(), conv, principal); err != nil {
			// run only returns errors that are not already handled (i.e., infra
			// errors that abort the turn). Log the detail server-side and emit
			// a generic code to the bus so no internal detail leaks to clients.
			h.logger.Error("harness: turn failed", "conversationId", conv, "err", err)
			h.bus.Publish(principal.UserID, events.Event{
				Type: "turn.failed",
				Data: TurnFailedPayload{Code: "infrastructure"},
			})
		}
	}()
	return nil
}

// ── run ──────────────────────────────────────────────────────────────────────

// run executes a full turn, potentially spanning multiple model rounds when the
// model requests tool calls. Each round:
//  1. Assembles the gateway request from persisted conversation state.
//  2. Streams the model reply, forwarding TextDelta events and accumulating ToolCalls.
//  3. On Done with no tool calls: persists the assistant message and emits
//     message.completed — turn ends.
//  4. On Done with tool calls: persists the assistant message (with tool_calls),
//     executes each call in order through the registry, persists each result, then
//     loops for the next round.
//
// The loop is bounded by h.stepCap. If the cap is reached, a graceful "unable to
// finish" assistant message is persisted and message.completed is emitted.
//
// Error handling: run classifies errors via classify() and acts accordingly.
//   - faultToolError: persisted as tool result, emits tool.failed, loop continues.
//   - faultContextLength: routed to handleContextLength (seam for context-assembly slice).
//   - faultInfrastructure: returned to StartTurn, which logs and emits turn.failed.
//
// run returns only infrastructure errors; ToolError and ErrContextLength are
// handled internally and never returned.
func (h *Harness) run(ctx context.Context, conv ConversationID, principal store.Principal) error {
	scope := store.UserScope(principal)
	sq := h.store.ForUser(scope)

	// Build a by-name lookup from the catalog for O(1) dispatch.
	tools := h.reg.Catalog()
	toolMap := make(map[string]capability.Tool, len(tools))
	for _, t := range tools {
		toolMap[t.Name] = t
	}

	// Map the capability catalog to gateway tool schemas for all rounds.
	gwTools := make([]gateway.ToolSchema, 0, len(tools))
	for _, t := range tools {
		gwTools = append(gwTools, gateway.ToolSchema{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Schema,
		})
	}

	for step := range h.stepCap {
		// Re-assemble from persisted state on every round so tool-result messages
		// are included in the context fed back to the model.
		req, err := h.assembleChatRequest(ctx, sq, conv, gwTools)
		if err != nil {
			return fmt.Errorf("harness: assemble request (step %d): %w", step, err)
		}

		seq, setupErr := h.gw.Chat(ctx, req)
		if setupErr != nil {
			return h.classifyGatewayError(ctx, sq, conv, principal.UserID, setupErr, step, "chat setup")
		}

		var textAccum []byte
		var calls []*gateway.ToolCall
		var streamErr error

		for chunk, chunkErr := range seq {
			if chunkErr != nil {
				streamErr = chunkErr
				break
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
			if chunk.ToolCall != nil {
				calls = append(calls, chunk.ToolCall)
			}
			if chunk.Done {
				break
			}
		}

		if streamErr != nil {
			return h.classifyGatewayError(ctx, sq, conv, principal.UserID, streamErr, step, "stream")
		}

		// Done with no tool calls: this is the final reply — persist and stop.
		if len(calls) == 0 {
			assistantID, err := persistAssistantMessage(ctx, sq, conv, string(textAccum), "")
			if err != nil {
				return fmt.Errorf("harness: persist final assistant message: %w", err)
			}
			h.bus.Publish(principal.UserID, events.Event{
				Type: "message.completed",
				Data: CompletedPayload{
					MessageID:    assistantID,
					FinishReason: "stop",
				},
			})
			return nil
		}

		// Done with tool calls: persist the assistant message (with tool_calls JSON),
		// then execute each call in order.
		toolCallsJSON, err := json.Marshal(calls)
		if err != nil {
			return fmt.Errorf("harness: marshal tool_calls (step %d): %w", step, err)
		}
		if _, err := persistAssistantMessage(ctx, sq, conv, string(textAccum), string(toolCallsJSON)); err != nil {
			return fmt.Errorf("harness: persist assistant message with tool_calls (step %d): %w", step, err)
		}

		// Execute tool calls in order, persisting each result immediately.
		for _, c := range calls {
			if err := h.executeAndPersistToolCall(ctx, sq, conv, principal.UserID, c, toolMap, scope); err != nil {
				// executeAndPersistToolCall only returns infrastructure errors;
				// ToolErrors are handled internally (persisted + tool.failed emitted).
				return fmt.Errorf("harness: execute tool call %q (step %d): %w", c.ID, step, err)
			}
		}
		// Loop: next iteration will re-assemble and call the model again.
	}

	// Step cap reached: persist a graceful message and end the turn.
	const capMessage = "I wasn't able to finish that — I reached the maximum number of steps. Please try again."
	assistantID, err := persistAssistantMessage(ctx, sq, conv, capMessage, "")
	if err != nil {
		return fmt.Errorf("harness: persist step-cap message: %w", err)
	}
	h.bus.Publish(principal.UserID, events.Event{
		Type: "message.completed",
		Data: CompletedPayload{
			MessageID:    assistantID,
			FinishReason: "step_cap",
		},
	})
	return nil
}

// classifyGatewayError classifies a gateway or stream error and routes accordingly.
// Returns nil when the faultContextLength seam handles it gracefully (persists a
// message and emits message.completed); propagates an infra error if the seam's
// own persistence fails. Returns the wrapped original error for faultInfrastructure
// so the caller can return it and trigger turn.failed.
func (h *Harness) classifyGatewayError(
	ctx context.Context,
	sq *store.ScopedQueries,
	conv, userID string,
	err error,
	step int,
	phase string,
) error {
	fault := classify(err)
	if fault == faultContextLength {
		// Route to the context-length seam. The context-assembly slice will implement
		// actual prompt trimming and retry here.
		return h.handleContextLength(ctx, sq, conv, userID)
	}
	// faultToolError and faultInfrastructure: a gateway returning a ToolError is
	// unusual (the gateway is not a tool), but both cases abort the turn the same
	// way — wrap and return so StartTurn logs the detail and emits turn.failed.
	return fmt.Errorf("harness: %s (step %d): %w", phase, step, err)
}

// handleContextLength is the seam for the context-assembly slice. It is called
// when the gateway returns ErrContextLength (prompt too long for the model's
// context window). The context-assembly slice will replace this stub with actual
// prompt trimming and retry logic.
//
// Current stub: produce a graceful "conversation too long" outcome so the turn
// ends cleanly rather than aborting with an opaque infrastructure error.
//
// context-assembly slice implements the actual trimming
func (h *Harness) handleContextLength(
	ctx context.Context,
	sq *store.ScopedQueries,
	conv, userID string,
) error {
	const msg = "This conversation has grown too long for me to continue. Please start a new conversation."
	assistantID, err := persistAssistantMessage(ctx, sq, conv, msg, "")
	if err != nil {
		// Persist failure is infrastructure — let it propagate.
		return fmt.Errorf("harness: persist context-length message: %w", err)
	}
	// Emit message.completed as the single terminal event for this turn. The
	// context-assembly slice can later trim-and-retry (no terminal event) or,
	// on irrecoverable overflow, fall through to this single-message.completed
	// graceful pattern. turn.failed must NOT also be emitted — clients treat
	// message.completed and turn.failed as mutually exclusive terminal events.
	h.bus.Publish(userID, events.Event{
		Type: "message.completed",
		Data: CompletedPayload{
			MessageID:    assistantID,
			FinishReason: "context_length",
		},
	})
	return nil
}

// ── assembly ─────────────────────────────────────────────────────────────────

// assembleChatRequest builds a gateway.ChatRequest from the persisted message
// history. It prepends a system prompt and round-trips each message type:
//   - role "user"      → gateway.Message{Role:"user", Content}
//   - role "assistant" with non-null tool_calls → gateway.Message{Role:"assistant", Content, ToolCalls}
//   - role "assistant" without tool_calls → gateway.Message{Role:"assistant", Content}
//   - role "tool"      → gateway.Message{Role:"tool", ToolCallID, Content}
func (h *Harness) assembleChatRequest(
	ctx context.Context,
	sq *store.ScopedQueries,
	conv ConversationID,
	gwTools []gateway.ToolSchema,
) (gateway.ChatRequest, error) {
	msgs, err := sq.ListMessages(ctx, conv)
	if err != nil {
		return gateway.ChatRequest{}, fmt.Errorf("list messages: %w", err)
	}

	const systemPrompt = "You are Qovira, a helpful personal assistant."
	gwMsgs := make([]gateway.Message, 0, len(msgs)+1)
	gwMsgs = append(gwMsgs, gateway.Message{Role: "system", Content: systemPrompt})

	for _, m := range msgs {
		switch m.Role {
		case "tool":
			gwMsgs = append(gwMsgs, gateway.Message{
				Role:       "tool",
				ToolCallID: m.ToolCallID.String,
				Content:    m.Content,
			})
		case "assistant":
			gm := gateway.Message{Role: "assistant", Content: m.Content}
			if m.ToolCalls.Valid && m.ToolCalls.String != "" {
				var tcs []gateway.ToolCall
				if jsonErr := json.Unmarshal([]byte(m.ToolCalls.String), &tcs); jsonErr != nil {
					// Malformed persisted JSON is a programming error; surface it.
					return gateway.ChatRequest{}, fmt.Errorf("unmarshal tool_calls for message %s: %w", m.ID, jsonErr)
				}
				gm.ToolCalls = tcs
			}
			gwMsgs = append(gwMsgs, gm)
		default:
			// "user" and any future roles pass through as plain content messages.
			gwMsgs = append(gwMsgs, gateway.Message{
				Role:    m.Role,
				Content: m.Content,
			})
		}
	}

	return gateway.ChatRequest{
		Messages: gwMsgs,
		Tools:    gwTools,
	}, nil
}

// ── tool execution ────────────────────────────────────────────────────────────

// executeAndPersistToolCall executes one tool call from the model, emits
// tool lifecycle events, and persists the result message.
//
// Error classification:
//   - If the model names an unknown tool: treated as a model fault (hallucinated
//     tool name), produces a *capability.ToolError so the model can self-correct.
//   - If tool.Execute returns a *capability.ToolError: persisted as tool-result
//     (model-safe message only), emits tool.failed, turn continues.
//   - If tool.Execute returns any other error: infrastructure — the error is
//     returned so the caller aborts the turn and emits turn.failed.
//
// This method returns only infrastructure errors; ToolError paths return nil.
func (h *Harness) executeAndPersistToolCall(
	ctx context.Context,
	sq *store.ScopedQueries,
	conv, userID string,
	call *gateway.ToolCall,
	toolMap map[string]capability.Tool,
	scope store.Scope,
) error {
	tool, found := toolMap[call.Name]

	risk := ""
	if found {
		risk = riskTierString(tool.Risk)
	}

	// Emit tool.started.
	h.bus.Publish(userID, events.Event{
		Type: "tool.started",
		Data: ToolStartedPayload{
			CallID:      call.ID,
			Name:        call.Name,
			Risk:        risk,
			ArgsSummary: argsSummary(call.Arguments),
		},
	})

	// Unknown tool: the model hallucinated a tool name — model fault, not infra.
	// Produce a *capability.ToolError so it flows through the same tool.failed + continue path.
	if !found {
		toolErr := &capability.ToolError{
			Code:    "unknown_tool",
			Message: "unknown tool: " + call.Name,
		}
		return h.persistToolError(ctx, sq, conv, userID, call.ID, toolErr)
	}

	result, execErr := tool.Execute(ctx, scope, call.Arguments)
	if execErr != nil {
		if classify(execErr) == faultToolError {
			// Model-visible error: persist model-safe message, emit tool.failed, continue.
			var toolErr *capability.ToolError
			// errors.As is guaranteed to succeed since classify returned faultToolError.
			_ = errors.As(execErr, &toolErr)
			return h.persistToolError(ctx, sq, conv, userID, call.ID, toolErr)
		}
		// faultContextLength and faultInfrastructure: context-length from a tool is
		// not something the model can self-correct, so treat both as infra — abort.
		return fmt.Errorf("execute tool %q: %w", call.Name, execErr)
	}

	// Persist tool-result message.
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal tool result for call %s: %w", call.ID, err)
	}
	if _, err := sq.InsertMessage(ctx, store.InsertMessageParams{
		ID:             generateID(),
		ConversationID: conv,
		Role:           "tool",
		Content:        string(resultJSON),
		ToolCallID:     call.ID,
	}); err != nil {
		return fmt.Errorf("persist tool result for call %s: %w", call.ID, err)
	}

	// Emit tool.completed.
	h.bus.Publish(userID, events.Event{
		Type: "tool.completed",
		Data: ToolCompletedPayload{
			CallID: call.ID,
			Result: result,
		},
	})

	return nil
}

// persistToolError persists a *capability.ToolError as the tool-result message
// and emits tool.failed with the model-safe message. The turn continues after
// this call (the caller returns nil). No internal error detail is exposed.
func (h *Harness) persistToolError(
	ctx context.Context,
	sq *store.ScopedQueries,
	conv, userID, callID string,
	toolErr *capability.ToolError,
) error {
	// Persist the tool-result with the model-safe message so the next round's
	// assembly feeds it back to the model and it can self-correct.
	resultJSON, err := json.Marshal(map[string]string{
		"error": toolErr.Message,
		"code":  toolErr.Code,
	})
	if err != nil {
		return fmt.Errorf("marshal tool error result for call %s: %w", callID, err)
	}
	if _, err := sq.InsertMessage(ctx, store.InsertMessageParams{
		ID:             generateID(),
		ConversationID: conv,
		Role:           "tool",
		Content:        string(resultJSON),
		ToolCallID:     callID,
	}); err != nil {
		return fmt.Errorf("persist tool error result for call %s: %w", callID, err)
	}

	// Emit tool.failed with only the model-safe message — no internal detail.
	h.bus.Publish(userID, events.Event{
		Type: "tool.failed",
		Data: ToolFailedPayload{
			CallID: callID,
			Error:  toolErr.Message,
		},
	})

	// Return nil so the loop continues — the model will self-correct.
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// persistAssistantMessage inserts the assistant reply into the messages table
// and returns the new message ID. toolCallsJSON is the JSON array string for the
// tool_calls column, or "" if the message has no tool calls.
func persistAssistantMessage(
	ctx context.Context,
	sq *store.ScopedQueries,
	conv, content, toolCallsJSON string,
) (string, error) {
	msgID := generateID()
	_, err := sq.InsertMessage(ctx, store.InsertMessageParams{
		ID:             msgID,
		ConversationID: conv,
		Role:           "assistant",
		Content:        content,
		ToolCalls:      toolCallsJSON,
		FinishReason:   "stop",
	})
	if err != nil {
		return "", err
	}
	return msgID, nil
}

// riskTierString returns the stable string label for a RiskTier.
func riskTierString(r capability.RiskTier) string {
	switch r {
	case capability.RiskRead:
		return "read"
	case capability.RiskWrite:
		return "write"
	case capability.RiskExternal:
		return "external"
	case capability.RiskDestructive:
		return "destructive"
	default:
		return "unknown"
	}
}

// maxArgsSummaryBytes is the maximum number of bytes included in the argsSummary
// field of a ToolStartedPayload. Longer JSON is truncated with "…".
const maxArgsSummaryBytes = 256

// argsSummary returns a compact JSON summary of the given raw arguments, truncated
// to maxArgsSummaryBytes on a valid UTF-8 rune boundary. Truncating at a raw byte
// offset can split a multi-byte rune and produce ill-formed UTF-8, which would
// corrupt the JSON payload sent to the UI via "tool.started".
func argsSummary(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	s := string(raw)
	if len(s) <= maxArgsSummaryBytes {
		return s
	}
	// Back up from the byte limit until we land on the start of a rune.
	cut := maxArgsSummaryBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
