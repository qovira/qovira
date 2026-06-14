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
	"time"
	"unicode/utf8"

	"github.com/qovira/qovira/internal/capability"
	"github.com/qovira/qovira/internal/events"
	"github.com/qovira/qovira/internal/gateway"
	"github.com/qovira/qovira/internal/store"
	"github.com/qovira/qovira/internal/store/db"
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

// defaultHistoryTokenBudget is the soft token budget for history messages when
// none is configured. The system prompt sits outside the budget.
const defaultHistoryTokenBudget = 50_000

// defaultMaxContextRetries is the maximum number of additional trim-and-retry
// attempts when the gateway returns ErrContextLength.
const defaultMaxContextRetries = 2

// Config holds boot-time configuration for the harness.
type Config struct {
	// StepCap limits the number of model-gateway rounds (including tool-call
	// loops) per turn. If <= 0, the default of 8 is applied. On reaching the
	// cap the turn ends gracefully with a final assistant message.
	StepCap int

	// HistoryTokenBudget is the soft token budget (chars/4 heuristic) for history
	// messages included in each assembled ChatRequest. The system prompt is always
	// included and does NOT count toward this budget. If <= 0, the default of
	// 50_000 is applied.
	HistoryTokenBudget int

	// MaxContextRetries is the maximum number of additional trim-and-retry
	// attempts when the gateway returns ErrContextLength. Each retry applies a
	// harder trim (drops one more oldest group). If <= 0, the default of 2 is
	// applied. After exhausting retries the turn emits a graceful
	// message.completed{finishReason:"context_length"}.
	MaxContextRetries int
}

// ── Harness ───────────────────────────────────────────────────────────────────

// Harness is the AI turn orchestrator. Obtain one via New; the zero value is not valid.
type Harness struct {
	reg                Cataloger
	gw                 Chatter
	store              *store.Store
	bus                events.Publisher
	stepCap            int
	historyTokenBudget int
	maxContextRetries  int
	now                func() time.Time
	logger             *slog.Logger
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
	return NewWithClock(reg, gw, st, bus, cfg, logger, nil)
}

// NewWithClock constructs a Harness with an injected clock for deterministic
// testing of time-dependent system-prompt content. When nowFn is nil, time.Now
// is used. All other parameters are identical to New.
func NewWithClock(reg Cataloger, gw Chatter, st *store.Store, bus events.Publisher, cfg Config, logger *slog.Logger, nowFn func() time.Time) *Harness {
	if logger == nil {
		logger = slog.Default()
	}
	if nowFn == nil {
		nowFn = time.Now
	}
	stepCap := cfg.StepCap
	if stepCap <= 0 {
		stepCap = defaultStepCap
	}
	budget := cfg.HistoryTokenBudget
	if budget <= 0 {
		budget = defaultHistoryTokenBudget
	}
	maxRetries := cfg.MaxContextRetries
	if maxRetries <= 0 {
		maxRetries = defaultMaxContextRetries
	}
	return &Harness{
		reg:                reg,
		gw:                 gw,
		store:              st,
		bus:                bus,
		stepCap:            stepCap,
		historyTokenBudget: budget,
		maxContextRetries:  maxRetries,
		now:                nowFn,
		logger:             logger,
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
	origin Origin,
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

		if err := h.run(context.Background(), conv, origin, principal); err != nil {
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
//   - faultContextLength: routed to handleContextLength — trims harder and retries,
//     bounded by h.maxContextRetries; exhausted retries emit a single graceful
//     message.completed{finishReason:"context_length"}.
//   - faultInfrastructure: returned to StartTurn, which logs and emits turn.failed.
//
// run returns only infrastructure errors; ToolError and ErrContextLength are
// handled internally and never returned.
func (h *Harness) run(ctx context.Context, conv ConversationID, origin Origin, principal store.Principal) error {
	scope := store.UserScope(principal)
	sq := h.store.ForUser(scope)

	// Load the user profile once per turn for the system prompt.
	profile, err := sq.GetProfile(ctx)
	if err != nil {
		return fmt.Errorf("harness: load user profile: %w", err)
	}

	// Build a by-name lookup from the full catalog for O(1) dispatch.
	// The full catalog (not the trust-filtered one) is used for the toolMap so that
	// the execution gate can enforce Block on any tool the model calls, even those
	// filtered from the offered catalog.
	allTools := h.reg.Catalog()
	toolMap := make(map[string]capability.Tool, len(allTools))
	for _, t := range allTools {
		toolMap[t.Name] = t
	}

	// Apply the advisory catalog filter: omit tools whose policy(risk, trust) == Block.
	// This is Layer 1 (advisory): the model is not offered blocked tools.
	// Layer 2 (execution gate in executeAndPersistToolCall) enforces Block even if
	// the model calls a filtered tool anyway.
	filteredTools := filterCatalogForTrust(allTools, origin.Trust)

	// Map the filtered capability catalog to gateway tool schemas for all rounds.
	gwTools := make([]gateway.ToolSchema, 0, len(filteredTools))
	for _, t := range filteredTools {
		gwTools = append(gwTools, gateway.ToolSchema{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Schema,
		})
	}

	// trimLevel controls how aggressively history is trimmed. It starts at 0 (soft
	// budget only) and is incremented by handleContextLength on each retry, causing
	// progressively more oldest groups to be dropped.
	trimLevel := 0

	for step := range h.stepCap {
		// runStep executes one model call (assemble → chat → stream → consume),
		// handling ErrContextLength at BOTH setup and stream levels with bounded
		// trim-and-retry inline — neither level advances the step counter. It returns:
		//   - result consumed (text + calls): caller processes the round.
		//   - termDone true: context-length retries exhausted; graceful terminal
		//     event already emitted; caller returns termErr immediately.
		//   - err non-nil: infra error; caller returns it to trigger turn.failed.
		sr := h.runStep(ctx, sq, conv, principal.UserID, gwTools, profile, &trimLevel, step)
		if sr.termDone {
			// Graceful context-length terminal event emitted (or persist failed).
			return sr.termErr
		}
		if sr.err != nil {
			return sr.err
		}

		// Done with no tool calls: this is the final reply — persist and stop.
		if len(sr.calls) == 0 {
			assistantID, err := persistAssistantMessage(ctx, sq, conv, string(sr.text), "")
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
		toolCallsJSON, err := json.Marshal(sr.calls)
		if err != nil {
			return fmt.Errorf("harness: marshal tool_calls (step %d): %w", step, err)
		}
		if _, err := persistAssistantMessage(ctx, sq, conv, string(sr.text), string(toolCallsJSON)); err != nil {
			return fmt.Errorf("harness: persist assistant message with tool_calls (step %d): %w", step, err)
		}

		// Execute tool calls in order, persisting each result immediately.
		for _, c := range sr.calls {
			done, err := h.executeAndPersistToolCall(ctx, sq, conv, principal.UserID, c, toolMap, scope, origin)
			if err != nil {
				// executeAndPersistToolCall only returns infrastructure errors;
				// ToolErrors and Block refusals are handled internally (persisted + event emitted).
				return fmt.Errorf("harness: execute tool call %q (step %d): %w", c.ID, step, err)
			}
			if done {
				// A Confirm decision suspended the turn. The turn goroutine ends here;
				// no terminal event is emitted (the turn is paused, not finished).
				// confirmation-suspend-resume slice persists the pending_confirmations row,
				// emits confirmation.required, and lets Resolve re-enter run.
				return nil
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

// stepResult is the outcome of one runStep call.
type stepResult struct {
	// text is the accumulated text content from the stream when err is nil
	// and termDone is false.
	text []byte
	// calls is the accumulated tool calls from the stream when err is nil
	// and termDone is false.
	calls []*gateway.ToolCall
	// err is a non-nil infrastructure error when set; caller returns it so
	// StartTurn can log and emit turn.failed.
	err error
	// termDone is true when a terminal event has already been emitted (graceful
	// context-length exhaust or its own persist failure). The caller must return
	// termErr immediately.
	termDone bool
	// termErr is the error from persist on context-length exhaust; nil on a clean
	// graceful terminal.
	termErr error
}

// runStep assembles the chat request, calls the gateway, and fully consumes the
// stream for one model round. It handles ErrContextLength at BOTH the setup
// level (Chat() returns an error before the stream) and the stream level (error
// yielded during iteration) using the same bounded trim-and-retry inner loop.
// Neither level advances the outer step counter — CL retries are "free" w.r.t.
// the step cap, bounded instead by h.maxContextRetries via handleContextLength.
//
// The returned stepResult carries exactly one meaningful outcome:
//   - text/calls populated, err nil, termDone false: consumed round; caller processes.
//   - err non-nil, termDone false: infra error; caller returns err.
//   - termDone true: context-length retries exhausted; graceful terminal already
//     emitted; caller returns termErr.
func (h *Harness) runStep(
	ctx context.Context,
	sq *store.ScopedQueries,
	conv ConversationID,
	userID string,
	gwTools []gateway.ToolSchema,
	profile db.User,
	trimLevel *int,
	step int,
) stepResult {
	// Inner retry loop for ErrContextLength at both setup and stream levels.
	// Each iteration re-assembles the request with the current trimLevel so
	// that harder trims on successive CL errors are applied correctly.
	for {
		req, err := h.assembleChatRequest(ctx, sq, conv, gwTools, profile, *trimLevel)
		if err != nil {
			return stepResult{err: fmt.Errorf("harness: assemble request (step %d): %w", step, err)}
		}

		seq, setupErr := h.gw.Chat(ctx, req)
		if setupErr != nil {
			if classify(setupErr) != faultContextLength {
				// Non-context-length error: infra abort.
				return stepResult{err: fmt.Errorf("harness: chat setup (step %d): %w", step, setupErr)}
			}
			// Setup-level ErrContextLength: increment trimLevel and check budget.
			done, tErr := h.handleContextLength(ctx, sq, conv, userID, trimLevel)
			if done {
				return stepResult{termDone: true, termErr: tErr}
			}
			// Budget not exhausted; loop with incremented trimLevel.
			continue
		}

		// Consume the stream. On a stream-level ErrContextLength, apply the same
		// trim-and-retry logic without returning to the outer step loop.
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
				h.bus.Publish(userID, events.Event{
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
			if classify(streamErr) == faultContextLength {
				// Stream-level ErrContextLength: same bounded retry as setup level.
				// Reset any partially accumulated deltas — they won't be resent on
				// retry since the client gets a fresh stream.
				textAccum = nil
				calls = nil
				done, tErr := h.handleContextLength(ctx, sq, conv, userID, trimLevel)
				if done {
					return stepResult{termDone: true, termErr: tErr}
				}
				// Budget not exhausted; loop with incremented trimLevel.
				continue
			}
			// Non-CL stream error: infra abort.
			return stepResult{err: classifyGatewayError(streamErr, step, "stream")}
		}

		// Stream consumed successfully.
		return stepResult{text: textAccum, calls: calls}
	}
}

// classifyGatewayError wraps a non-context-length gateway or stream error for
// return to StartTurn as an infrastructure abort. Context-length errors are
// handled by the caller before reaching here; this function only sees
// faultToolError and faultInfrastructure. Both abort the turn the same way:
// a gateway returning a ToolError is unusual but treated as infrastructure.
func classifyGatewayError(err error, step int, phase string) error {
	return fmt.Errorf("harness: %s (step %d): %w", phase, step, err)
}

// handleContextLength implements the bounded trim-and-retry logic for
// ErrContextLength. It updates *trimLevel and returns:
//   - (false, nil) when a retry should proceed (trimLevel was incremented).
//   - (true, nil) when retries are exhausted and the graceful terminal event
//     has been emitted. The caller must return termErr (nil here) to StartTurn.
//   - (true, err) when the graceful-message persist itself fails (infra error).
//
// This method never emits turn.failed — it always either allows a retry or
// emits exactly one message.completed. The single-terminal-event invariant is
// maintained by the caller (run), which returns immediately after (true, _).
func (h *Harness) handleContextLength(
	ctx context.Context,
	sq *store.ScopedQueries,
	conv, userID string,
	trimLevel *int,
) (done bool, err error) {
	// Increment the trim level so the next assembleChatRequest drops one more
	// oldest group.
	*trimLevel++

	// If we have not yet exhausted the retry budget, allow the caller to retry.
	if *trimLevel <= h.maxContextRetries {
		return false, nil
	}

	// Retries exhausted. Emit exactly one graceful terminal event.
	const msg = "This conversation has grown too long for me to continue. Please start a new conversation."
	assistantID, persistErr := persistAssistantMessage(ctx, sq, conv, msg, "")
	if persistErr != nil {
		// Persist failure is infrastructure — propagate; StartTurn emits turn.failed.
		return true, fmt.Errorf("harness: persist context-length message: %w", persistErr)
	}
	// Emit message.completed as the sole terminal event. turn.failed must NOT be
	// emitted alongside it — clients treat the two as mutually exclusive.
	h.bus.Publish(userID, events.Event{
		Type: "message.completed",
		Data: CompletedPayload{
			MessageID:    assistantID,
			FinishReason: "context_length",
		},
	})
	return true, nil
}

// ── assembly ─────────────────────────────────────────────────────────────────

// assembleChatRequest builds a gateway.ChatRequest from the persisted message
// history. It:
//  1. Composes a per-turn system prompt (identity + user context + memory slot).
//  2. Loads and round-trips each persisted message (user/assistant/tool roles).
//  3. Applies the sliding-window trim so that only history fitting within
//     h.historyTokenBudget estimated tokens is included (oldest groups dropped
//     first; newest group is always kept to satisfy the boundary rule).
//  4. Applies extraDrop = trimLevel additional group drops for harder trims on
//     context-length retries.
//
// The system prompt sits outside the history budget and is always included.
//
// Message role round-trip:
//   - role "user"      → gateway.Message{Role:"user", Content}
//   - role "assistant" with non-null tool_calls → {Role:"assistant", Content, ToolCalls}
//   - role "assistant" without tool_calls → {Role:"assistant", Content}
//   - role "tool"      → {Role:"tool", ToolCallID, Content}
func (h *Harness) assembleChatRequest(
	ctx context.Context,
	sq *store.ScopedQueries,
	conv ConversationID,
	gwTools []gateway.ToolSchema,
	profile db.User,
	trimLevel int,
) (gateway.ChatRequest, error) {
	msgs, err := sq.ListMessages(ctx, conv)
	if err != nil {
		return gateway.ChatRequest{}, fmt.Errorf("list messages: %w", err)
	}

	// Convert persisted messages to gateway messages (faithful role round-trip).
	rawMsgs := make([]gateway.Message, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			rawMsgs = append(rawMsgs, gateway.Message{
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
			rawMsgs = append(rawMsgs, gm)
		default:
			// "user" and any future roles pass through as plain content messages.
			rawMsgs = append(rawMsgs, gateway.Message{
				Role:    m.Role,
				Content: m.Content,
			})
		}
	}

	// Apply the sliding-window trim. trimLevel > 0 drops additional oldest groups
	// on context-length retries (harder trim on each attempt).
	trimmed := trimToWindowBudget(rawMsgs, h.historyTokenBudget, trimLevel)

	// Compose the per-turn system prompt (outside the budget).
	systemPrompt := buildSystemPrompt(h.now(), profile)

	gwMsgs := make([]gateway.Message, 0, len(trimmed)+1)
	gwMsgs = append(gwMsgs, gateway.Message{Role: "system", Content: systemPrompt})
	gwMsgs = append(gwMsgs, trimmed...)

	return gateway.ChatRequest{
		Messages: gwMsgs,
		Tools:    gwTools,
	}, nil
}

// ── tool execution ────────────────────────────────────────────────────────────

// executeAndPersistToolCall executes one tool call from the model, emits
// tool lifecycle events, and persists the result message.
//
// The second return value, done, is true when a Confirm decision has suspended
// the turn (the caller must stop processing further calls and return without
// emitting a terminal event). It is always false for Auto and Block decisions.
//
// Error classification:
//   - If the model names an unknown tool: treated as a model fault (hallucinated
//     tool name), produces a *capability.ToolError so the model can self-correct.
//   - If policy(tool.Risk, origin.Trust) == Block: persist a model-visible "not
//     permitted" refusal as the tool-result, emit tool.failed, loop continues
//     (done=false, nil error).
//   - If policy == Confirm: route to requestConfirmation seam — suspends the turn;
//     returns (true, nil) so the caller stops without a terminal event.
//   - If tool.Execute returns a *capability.ToolError: persisted as tool-result
//     (model-safe message only), emits tool.failed, turn continues.
//   - If tool.Execute returns any other error: infrastructure — the error is
//     returned so the caller aborts the turn and emits turn.failed.
//
// This method returns only infrastructure errors; ToolError and Block paths return nil.
func (h *Harness) executeAndPersistToolCall(
	ctx context.Context,
	sq *store.ScopedQueries,
	conv, userID string,
	call *gateway.ToolCall,
	toolMap map[string]capability.Tool,
	scope store.Scope,
	origin Origin,
) (done bool, err error) {
	tool, found := toolMap[call.Name]

	// Unknown tool: the model hallucinated a tool name — model fault, not infra.
	// Emit tool.started (the model made an attempt) then produce a *capability.ToolError
	// so it flows through the same tool.failed + continue path.
	// Unknown tools have no risk tier, so the policy gate is skipped (no tier to check).
	if !found {
		h.bus.Publish(userID, events.Event{
			Type: "tool.started",
			Data: ToolStartedPayload{
				CallID:      call.ID,
				Name:        call.Name,
				Risk:        "",
				ArgsSummary: argsSummary(call.Arguments),
			},
		})
		toolErr := &capability.ToolError{
			Code:    "unknown_tool",
			Message: "unknown tool: " + call.Name,
		}
		return false, h.persistToolError(ctx, sq, conv, userID, call.ID, toolErr)
	}

	risk := riskTierString(tool.Risk)

	// ── Policy gate (execution gate — Layer 2) ────────────────────────────────
	// This is the real enforcement boundary. Even if a tool leaked through the
	// catalog filter (Layer 1), the execution gate refuses it here.
	//
	// tool.started is emitted AFTER the policy decision so that:
	//   - Auto: tool.started fires immediately before execution, always paired with
	//     tool.completed or tool.failed.
	//   - Block: no tool.started — this is a refusal, not an attempt; the UI sees
	//     only tool.failed with the model-visible refusal message.
	//   - Confirm: no tool.started — the turn suspends here; confirmation.required
	//     belongs to the confirmation-suspend-resume slice and is emitted there.
	//     Emitting tool.started without a resolving tool.completed/tool.failed would
	//     leave a permanently spinning UI chip.
	switch policy(tool.Risk, origin.Trust) {
	case Block:
		// Not permitted from this source: persist a model-visible refusal as the
		// tool-result so the model sees it and the loop continues. No tool.started
		// is emitted — Block is a refusal, not an execution attempt.
		toolErr := &capability.ToolError{
			Code:    "not_permitted",
			Message: "this operation is not permitted from the current source",
		}
		return false, h.persistToolError(ctx, sq, conv, userID, call.ID, toolErr)

	case Confirm:
		// Route to the confirmation seam. No tool.started is emitted here: the turn
		// suspends without executing the tool, and the next slice
		// (confirmation-suspend-resume) will persist pending_confirmations, emit
		// confirmation.required, and let Resolve re-enter run.
		return h.requestConfirmation(ctx, sq, conv, userID, call, tool)

	case Auto:
		// Fall through to execution below.
	}

	// Emit tool.started immediately before execution so it is always paired with
	// either tool.completed (success) or tool.failed (ToolError).
	h.bus.Publish(userID, events.Event{
		Type: "tool.started",
		Data: ToolStartedPayload{
			CallID:      call.ID,
			Name:        call.Name,
			Risk:        risk,
			ArgsSummary: argsSummary(call.Arguments),
		},
	})

	result, execErr := tool.Execute(ctx, scope, call.Arguments)
	if execErr != nil {
		if classify(execErr) == faultToolError {
			// Model-visible error: persist model-safe message, emit tool.failed, continue.
			var toolErr *capability.ToolError
			// errors.As is guaranteed to succeed since classify returned faultToolError.
			_ = errors.As(execErr, &toolErr)
			return false, h.persistToolError(ctx, sq, conv, userID, call.ID, toolErr)
		}
		// faultContextLength and faultInfrastructure: context-length from a tool is
		// not something the model can self-correct, so treat both as infra — abort.
		return false, fmt.Errorf("execute tool %q: %w", call.Name, execErr)
	}

	// Persist tool-result message.
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return false, fmt.Errorf("marshal tool result for call %s: %w", call.ID, err)
	}
	if _, err := sq.InsertMessage(ctx, store.InsertMessageParams{
		ID:             generateID(),
		ConversationID: conv,
		Role:           "tool",
		Content:        string(resultJSON),
		ToolCallID:     call.ID,
	}); err != nil {
		return false, fmt.Errorf("persist tool result for call %s: %w", call.ID, err)
	}

	// Emit tool.completed.
	h.bus.Publish(userID, events.Event{
		Type: "tool.completed",
		Data: ToolCompletedPayload{
			CallID: call.ID,
			Result: result,
		},
	})

	return false, nil
}

// requestConfirmation is the seam for the confirmation-suspend-resume slice.
// It suspends the current turn by returning (true, nil) — the caller stops
// processing further tool calls and returns without emitting a terminal event.
//
// In this slice (gate-tool-execution-by-risk-and-trust) the seam does NOT:
//   - persist a pending_confirmations row
//   - emit confirmation.required
//   - fabricate a tool result
//
// The next slice fills this function with the full suspend/resume logic.
// confirmation-suspend-resume slice persists the pending_confirmations row, emits
// confirmation.required, and lets Resolve re-enter run.
func (h *Harness) requestConfirmation(
	_ context.Context,
	_ *store.ScopedQueries,
	_ string,
	_ string,
	_ *gateway.ToolCall,
	_ capability.Tool,
) (done bool, err error) {
	// Suspend the turn without a terminal event.
	return true, nil
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
