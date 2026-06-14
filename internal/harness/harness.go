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
	"slices"
	"time"
	"unicode/utf8"

	sqlite3 "github.com/omnilium/go-sqlcipher"

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

// ConfirmationRequiredPayload is the Data for a "confirmation.required" event,
// emitted when a Confirm-tier tool call is encountered and a pending_confirmations
// row has been created. The client should present the user with an approve/deny
// choice and POST to .../confirmations/{callId}.
type ConfirmationRequiredPayload struct {
	// CallID is the gateway tool call ID (= the pending_confirmations row ID).
	// This is the API-addressable identifier used in POST .../confirmations/{callId}.
	CallID string `json:"callId"`
	// Name is the tool name.
	Name string `json:"name"`
	// Risk is the risk tier string (e.g. "external", "destructive").
	Risk string `json:"risk"`
	// Args is the raw JSON arguments of the tool call.
	Args json.RawMessage `json:"args"`
	// ExpiresAt is the RFC 3339 UTC timestamp after which this confirmation expires.
	ExpiresAt string `json:"expiresAt"`
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

// defaultConfirmationTTL is the default time-to-live for a pending_confirmations
// row when Config.ConfirmationTTL is zero.
const defaultConfirmationTTL = 24 * time.Hour

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

	// ConfirmationTTL is the time-to-live for a pending_confirmations row. After
	// this duration, the confirmation expires. If <= 0, the default of 24h is
	// applied. Expiry enforcement (lazy check + scheduler sweep) is a future slice.
	ConfirmationTTL time.Duration
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
	confirmationTTL    time.Duration
	now                func() time.Time
	logger             *slog.Logger
	convLocks          *convLocks // per-conversation run serialisation
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
	confirmTTL := cfg.ConfirmationTTL
	if confirmTTL <= 0 {
		confirmTTL = defaultConfirmationTTL
	}
	return &Harness{
		reg:                reg,
		gw:                 gw,
		store:              st,
		bus:                bus,
		stepCap:            stepCap,
		historyTokenBudget: budget,
		maxContextRetries:  maxRetries,
		confirmationTTL:    confirmTTL,
		now:                nowFn,
		logger:             logger,
		convLocks:          newConvLocks(),
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
		// Per-conversation serialisation: at most one run executes per conversationID
		// at a time. Acquire increments the refcount; the entry mutex is acquired
		// outside the guard mutex so a long-running turn never blocks other conversations.
		entry := h.convLocks.acquire(conv)
		entry.mu.Lock()
		defer func() {
			entry.mu.Unlock()
			h.convLocks.release(conv)
		}()

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
// model requests tool calls. Each step:
//  1. Checks if the last persisted assistant message has outstanding (unresolved)
//     tool calls. If so, processes those calls idempotently (resume path) —
//     without calling the gateway — and either suspends or continues.
//  2. When no outstanding calls: assembles the gateway request, streams the model
//     reply, and processes the result (text-only → persists + completes; with
//     tool calls → persists assistant msg + processes calls idempotently).
//
// The loop is bounded by h.stepCap. If the cap is reached, a graceful "unable to
// finish" assistant message is persisted and message.completed is emitted.
//
// Error handling: run classifies errors via classify() and acts accordingly.
//   - faultToolError: persisted as tool result, emits tool.failed, loop continues.
//   - faultContextLength: routed to handleContextLength — trims harder and retries,
//     bounded by h.maxContextRetries; exhausted retries emit a single graceful
//     message.completed{finishReason:"context_length"}.
//   - faultInfrastructure: returned to StartTurn/Resolve, which logs and emits turn.failed.
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
		// ── Resume path: process outstanding tool calls from the last assistant message ──
		// Before calling the gateway, check whether the last assistant message has
		// tool_calls with unresolved results. This handles the re-entry case from
		// Resolve: the conversation is in mid-round state (assistant message persisted
		// but tool calls not fully resolved). Process them idempotently first.
		pendingCalls, _, err := outstandingToolCalls(ctx, sq, conv)
		if err != nil {
			return fmt.Errorf("harness: check outstanding tool calls (step %d): %w", step, err)
		}
		if len(pendingCalls) > 0 {
			// There are unresolved tool calls from a previous (persisted) round.
			// Process them idempotently without calling the gateway.
			suspended, procErr := h.processToolCalls(ctx, sq, conv, principal.UserID, pendingCalls, toolMap, scope, origin, step)
			if procErr != nil {
				return procErr
			}
			if suspended {
				return nil // turn suspends again (some calls still pending)
			}
			// All resolved: fall through to the next gateway call (same step counter).
		} else {
			// ── Turn-completion guard ─────────────────────────────────────────────────
			// Before calling the gateway, verify the conversation actually needs a new
			// round. If the last persisted message is already a final assistant reply
			// (role="assistant" with no tool_calls), the turn is complete and this is a
			// spurious re-entry (e.g. G2 acquired the lock after G1 already finished the
			// turn). Return immediately without emitting any event — the lock is still
			// held here, so the check and the gate are atomic w.r.t. all other goroutines.
			done, guardErr := isTurnComplete(ctx, sq, conv)
			if guardErr != nil {
				return fmt.Errorf("harness: turn-completion guard (step %d): %w", step, guardErr)
			}
			if done {
				return nil // turn already finished by a concurrent runner — no-op
			}

			// ── Normal path: call the gateway for a new model round ──────────────────
			//
			// runStep executes one model call (assemble → chat → stream → consume),
			// handling ErrContextLength at BOTH setup and stream levels with bounded
			// trim-and-retry inline.
			sr := h.runStep(ctx, sq, conv, principal.UserID, gwTools, profile, &trimLevel, step)
			if sr.termDone {
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
			// then process each call idempotently.
			toolCallsJSON, err := json.Marshal(sr.calls)
			if err != nil {
				return fmt.Errorf("harness: marshal tool_calls (step %d): %w", step, err)
			}
			if _, err := persistAssistantMessage(ctx, sq, conv, string(sr.text), string(toolCallsJSON)); err != nil {
				return fmt.Errorf("harness: persist assistant message with tool_calls (step %d): %w", step, err)
			}

			// Process tool calls using the idempotent, state-driven loop.
			suspended, procErr := h.processToolCalls(ctx, sq, conv, principal.UserID, sr.calls, toolMap, scope, origin, step)
			if procErr != nil {
				return procErr
			}
			if suspended {
				// Turn is suspended — waiting for user confirmation(s). The turn goroutine
				// ends here; no terminal event is emitted. Resume is via Resolve.
				return nil
			}
		}
		// Loop: next iteration will check for outstanding calls or call the gateway.
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

// processToolCalls executes the idempotent, state-driven tool-call loop for one
// model round. It skips calls that already have a tool-result message (idempotent
// re-entry), handles Confirm rows by their status, and suspends if any call is
// still waiting. Returns (suspended=true, nil) when the turn suspends; returns
// (false, nil) when all calls are handled and the outer loop should continue;
// returns (false, err) on an infrastructure error.
func (h *Harness) processToolCalls(
	ctx context.Context,
	sq *store.ScopedQueries,
	conv, userID string,
	calls []*gateway.ToolCall,
	toolMap map[string]capability.Tool,
	scope store.Scope,
	origin Origin,
	step int,
) (suspended bool, err error) {
	// Build a set of call IDs that already have a persisted tool-result message.
	// This is the idempotency check — re-entry after resume skips already-done calls.
	doneIDs, err := toolResultSet(ctx, sq, conv)
	if err != nil {
		return false, fmt.Errorf("harness: load tool-result set (step %d): %w", step, err)
	}

	allHandled := true
	for _, c := range calls {
		// Skip if already has a result (idempotent on re-entry).
		if doneIDs[c.ID] {
			continue
		}

		// Look up the tool once — reused for the policy gate, the unknown-tool path,
		// and execution below. Avoids triple map lookup from the original code.
		tool, found := toolMap[c.Name]

		dec := policy(toolRisk(c.Name, toolMap), origin.Trust)

		// Unknown tools: no risk tier; skips the policy gate. Emit tool.started and
		// persist a model-visible "unknown tool" error so the model can self-correct.
		if !found {
			// Emit tool.started for the unknown tool (it was an attempt, just hallucinated).
			h.bus.Publish(userID, events.Event{
				Type: "tool.started",
				Data: ToolStartedPayload{
					CallID:      c.ID,
					Name:        c.Name,
					Risk:        "",
					ArgsSummary: argsSummary(c.Arguments),
				},
			})
			unknownErr := &capability.ToolError{
				Code:    "unknown_tool",
				Message: "unknown tool: " + c.Name,
			}
			if persistErr := h.persistToolError(ctx, sq, conv, userID, c.ID, unknownErr); persistErr != nil {
				return false, fmt.Errorf("harness: persist unknown tool error for call %q (step %d): %w", c.ID, step, persistErr)
			}
			doneIDs[c.ID] = true
			continue
		}

		switch dec {
		case Auto:
			// Execute and persist.
			if execErr := h.executeToolAndPersist(ctx, sq, conv, userID, c, toolMap, scope); execErr != nil {
				return false, fmt.Errorf("harness: execute tool call %q (step %d): %w", c.ID, step, execErr)
			}
			doneIDs[c.ID] = true

		case Block:
			// Persist a model-visible refusal.
			if execErr := h.persistBlockRefusal(ctx, sq, conv, userID, c.ID); execErr != nil {
				return false, fmt.Errorf("harness: persist block refusal for call %q (step %d): %w", c.ID, step, execErr)
			}
			doneIDs[c.ID] = true

		case Confirm:
			// Look up the pending_confirmations row for this call.
			row, getErr := sq.GetPendingConfirmation(ctx, c.ID)
			if getErr != nil {
				if errors.Is(getErr, store.ErrConfirmationNotFound) {
					// No row yet: create one and emit confirmation.required.
					// Reuse the `tool` variable looked up above — avoids a fourth map access.
					if insertErr := h.insertPendingConfirmation(ctx, sq, conv, userID, c, tool); insertErr != nil {
						return false, fmt.Errorf("harness: insert pending confirmation for call %q: %w", c.ID, insertErr)
					}
					allHandled = false // waiting for user
				} else {
					return false, fmt.Errorf("harness: get pending confirmation for call %q: %w", c.ID, getErr)
				}
			} else {
				switch row.Status {
				case "pending":
					allHandled = false // still waiting

				case "approved":
					// Execute the tool now that it is approved.
					if execErr := h.executeToolAndPersist(ctx, sq, conv, userID, c, toolMap, scope); execErr != nil {
						return false, fmt.Errorf("harness: execute approved tool call %q (step %d): %w", c.ID, step, execErr)
					}
					doneIDs[c.ID] = true

				case "denied":
					// Persist a synthetic "declined by the user" result and emit tool.failed.
					if persistErr := h.persistDeclinedResult(ctx, sq, conv, userID, c.ID); persistErr != nil {
						return false, fmt.Errorf("harness: persist declined result for call %q: %w", c.ID, persistErr)
					}
					doneIDs[c.ID] = true

				default:
					// Unknown/expired status: do not execute. Treat as still-pending
					// (expiry enforcement is a future slice).
					allHandled = false
				}
			}
		}
	}

	return !allHandled, nil
}

// toolRisk returns the RiskTier of the named tool, or a default Confirm tier
// for unknown tools (the unknown-tool path is handled before this is called in practice).
func toolRisk(name string, toolMap map[string]capability.Tool) capability.RiskTier {
	if t, ok := toolMap[name]; ok {
		return t.Risk
	}
	return capability.RiskRead // unknown tools bypass the confirm gate above
}

// outstandingToolCalls inspects the persisted message history to determine whether
// the last assistant message has tool_calls entries that do not yet have a
// corresponding tool-result message. It returns the outstanding calls (in original
// order) and the assistant message ID. If there are no outstanding calls (either
// because the last message is not an assistant+tool_calls message, or all calls
// already have results), the returned slice is empty.
func outstandingToolCalls(ctx context.Context, sq *store.ScopedQueries, conv string) ([]*gateway.ToolCall, string, error) {
	msgs, err := sq.ListMessages(ctx, conv)
	if err != nil {
		return nil, "", fmt.Errorf("list messages for outstanding calls: %w", err)
	}
	if len(msgs) == 0 {
		return nil, "", nil
	}

	// Build a set of tool call IDs that already have results.
	resultIDs := make(map[string]bool)
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID.Valid && m.ToolCallID.String != "" {
			resultIDs[m.ToolCallID.String] = true
		}
	}

	// Find the last assistant message that has tool_calls.
	for _, m := range slices.Backward(msgs) {
		if m.Role != "assistant" || !m.ToolCalls.Valid || m.ToolCalls.String == "" {
			continue
		}
		var tcs []gateway.ToolCall
		if jsonErr := json.Unmarshal([]byte(m.ToolCalls.String), &tcs); jsonErr != nil {
			return nil, "", fmt.Errorf("unmarshal tool_calls for message %s: %w", m.ID, jsonErr)
		}
		// Collect the outstanding (unresolved) calls.
		var outstanding []*gateway.ToolCall
		for j := range tcs {
			if !resultIDs[tcs[j].ID] {
				tc := tcs[j] // copy so we can take address
				outstanding = append(outstanding, &tc)
			}
		}
		if len(outstanding) == 0 {
			// This assistant message's calls are all done — not a suspended state.
			return nil, "", nil
		}
		return outstanding, m.ID, nil
	}
	return nil, "", nil
}

// isTurnComplete reports whether the turn for the given conversation has already
// ended. A turn is complete when the last persisted message is an assistant
// message with no tool_calls — a final reply. Callers use this as an idempotent
// re-entry guard: if true, run must return immediately without calling the
// gateway or emitting any event.
//
// The three non-complete cases where a gateway round IS required:
//   - Last message is "user" → fresh turn, not yet handled.
//   - Last message is "tool" → tool results are waiting for a continue round.
//   - Last message is "assistant" with tool_calls → outstanding calls still being
//     processed (handled by the outstanding-calls path above this call site).
//
// Calling this while holding the per-conversation lock guarantees that the check
// and the subsequent gateway call are atomic w.r.t. all other goroutines.
func isTurnComplete(ctx context.Context, sq *store.ScopedQueries, conv string) (bool, error) {
	msgs, err := sq.ListMessages(ctx, conv)
	if err != nil {
		return false, fmt.Errorf("list messages for turn-completion guard: %w", err)
	}
	if len(msgs) == 0 {
		return false, nil
	}
	last := msgs[len(msgs)-1]
	if last.Role != "assistant" {
		return false, nil
	}
	// An assistant message with tool_calls is not yet complete — tool results are
	// still outstanding (the resume path above handles this before we reach here,
	// so in practice this branch is not reached, but guard it for correctness).
	if last.ToolCalls.Valid && last.ToolCalls.String != "" {
		return false, nil
	}
	// Last message is an assistant final reply (no tool_calls) — turn is done.
	return true, nil
}

// toolResultSet builds a set of tool call IDs that already have a persisted
// tool-result message in the conversation. This drives the idempotency check in
// processToolCalls.
func toolResultSet(ctx context.Context, sq *store.ScopedQueries, conv string) (map[string]bool, error) {
	msgs, err := sq.ListMessages(ctx, conv)
	if err != nil {
		return nil, fmt.Errorf("list messages for tool-result set: %w", err)
	}
	set := make(map[string]bool)
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID.Valid && m.ToolCallID.String != "" {
			set[m.ToolCallID.String] = true
		}
	}
	return set, nil
}

// executeToolAndPersist executes a known tool (Auto or approved Confirm) and
// persists the result. It emits tool.started before execution and tool.completed
// (or tool.failed for ToolError) after. It does NOT apply a policy gate —
// callers are responsible for ensuring the decision is Auto or approved.
func (h *Harness) executeToolAndPersist(
	ctx context.Context,
	sq *store.ScopedQueries,
	conv, userID string,
	call *gateway.ToolCall,
	toolMap map[string]capability.Tool,
	scope store.Scope,
) error {
	tool, found := toolMap[call.Name]
	if !found {
		// Should not happen — callers check toolMap before calling this.
		return fmt.Errorf("executeToolAndPersist: tool %q not found in map", call.Name)
	}
	risk := riskTierString(tool.Risk)

	// Emit tool.started immediately before execution.
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
			var toolErr *capability.ToolError
			_ = errors.As(execErr, &toolErr)
			return h.persistToolError(ctx, sq, conv, userID, call.ID, toolErr)
		}
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
		// Defense-in-depth: the partial UNIQUE index on (conversation_id, tool_call_id)
		// fires when two paths race to persist the same tool result. With the per-
		// conversation lock in place this should never occur in practice; treat it as
		// "another path already persisted the result" and return cleanly so the turn
		// continues without double-emitting tool.completed.
		if isToolResultUniqueViolation(err) {
			return nil
		}
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

// persistBlockRefusal persists a model-visible "not permitted" refusal result
// for a Block-policy tool call and emits tool.failed. No tool.started is emitted
// (Block is a refusal, not an execution attempt).
func (h *Harness) persistBlockRefusal(
	ctx context.Context,
	sq *store.ScopedQueries,
	conv, userID, callID string,
) error {
	toolErr := &capability.ToolError{
		Code:    "not_permitted",
		Message: "this operation is not permitted from the current source",
	}
	return h.persistToolError(ctx, sq, conv, userID, callID, toolErr)
}

// insertPendingConfirmation persists a pending_confirmations row for a Confirm-tier
// tool call and emits the confirmation.required event. Called only when the row
// does not yet exist.
func (h *Harness) insertPendingConfirmation(
	ctx context.Context,
	sq *store.ScopedQueries,
	conv, userID string,
	call *gateway.ToolCall,
	tool capability.Tool,
) error {
	// Look up the assistant message ID that holds this tool call. It is the most
	// recent assistant message in this conversation that has tool_calls.
	msgID, err := findAssistantMessageForCall(ctx, sq, conv, call.ID)
	if err != nil {
		return fmt.Errorf("find assistant message for call %q: %w", call.ID, err)
	}

	expiresAt := h.now().UTC().Add(h.confirmationTTL).Format(time.RFC3339)
	argsJSON := string(call.Arguments)
	if argsJSON == "" {
		argsJSON = "{}"
	}
	risk := riskTierString(tool.Risk)

	if _, insertErr := sq.InsertPendingConfirmation(ctx, store.InsertPendingConfirmationParams{
		ID:             call.ID,
		ConversationID: conv,
		MessageID:      msgID,
		ToolName:       call.Name,
		Args:           argsJSON,
		Risk:           risk,
		Status:         "pending",
		ExpiresAt:      expiresAt,
	}); insertErr != nil {
		return fmt.Errorf("insert pending_confirmation: %w", insertErr)
	}

	h.bus.Publish(userID, events.Event{
		Type: "confirmation.required",
		Data: ConfirmationRequiredPayload{
			CallID:    call.ID,
			Name:      call.Name,
			Risk:      risk,
			Args:      call.Arguments,
			ExpiresAt: expiresAt,
		},
	})

	return nil
}

// persistDeclinedResult persists a synthetic "declined by the user" tool-result
// message and emits tool.failed. This is the model-visible signal that the user
// denied the confirmation, giving the model one round to acknowledge.
func (h *Harness) persistDeclinedResult(
	ctx context.Context,
	sq *store.ScopedQueries,
	conv, userID, callID string,
) error {
	const declinedMsg = "this action was declined by the user"
	toolErr := &capability.ToolError{
		Code:    "declined_by_user",
		Message: declinedMsg,
	}
	return h.persistToolError(ctx, sq, conv, userID, callID, toolErr)
}

// findAssistantMessageForCall finds the ID of the most recent assistant message
// in the conversation that contains a tool_calls array. This is the parent message
// for the pending_confirmations row.
func findAssistantMessageForCall(ctx context.Context, sq *store.ScopedQueries, conv, callID string) (string, error) {
	msgs, err := sq.ListMessages(ctx, conv)
	if err != nil {
		return "", fmt.Errorf("list messages: %w", err)
	}
	// Walk backwards: the most recently persisted assistant message with tool_calls
	// is the one that triggered the Confirm path.
	for _, m := range slices.Backward(msgs) {
		if m.Role != "assistant" || !m.ToolCalls.Valid || m.ToolCalls.String == "" {
			continue
		}
		// Verify this message actually contains the call ID.
		var tcs []gateway.ToolCall
		if jsonErr := json.Unmarshal([]byte(m.ToolCalls.String), &tcs); jsonErr != nil {
			continue
		}
		for _, tc := range tcs {
			if tc.ID == callID {
				return m.ID, nil
			}
		}
	}
	return "", fmt.Errorf("no assistant message found containing tool call %q", callID)
}

// ── Resolve ───────────────────────────────────────────────────────────────────

// ErrConfirmationNotFound is returned by Resolve when the callID does not exist
// for this user (or the conversationID doesn't match). Callers map this to HTTP 404.
var ErrConfirmationNotFound = store.ErrConfirmationNotFound

// ErrConfirmationAlreadyResolved is returned by Resolve when the pending row's
// status is not "pending". Callers map this to HTTP 409.
var ErrConfirmationAlreadyResolved = store.ErrConfirmationAlreadyResolved

// Resolve updates the status of a pending_confirmations row (approved or denied)
// and re-enters run asynchronously to resume the suspended turn. The HTTP handler
// returns 202 immediately; the resumed turn streams events over the bus.
//
// Resolve is a public surface alongside StartTurn; it is called by the
// POST /api/v1/conversations/{id}/confirmations/{callId} handler.
//
// convID is the conversation ID from the URL path. Resolve verifies that the
// pending_confirmations row belongs to that conversation, returning
// ErrConfirmationNotFound if the callID belongs to a different conversation.
//
// Error classification:
//   - ErrConfirmationNotFound: callID doesn't exist for this user or belongs to a
//     different conversation than convID → handler returns 404.
//   - ErrConfirmationAlreadyResolved: the CAS UPDATE found rowsAffected=0 (another
//     concurrent Resolve already won the race) → handler returns 409.
//   - Any other error: infrastructure → handler returns 500.
func (h *Harness) Resolve(ctx context.Context, convID, callID string, approved bool, principal store.Principal) error {
	scope := store.UserScope(principal)
	sq := h.store.ForUser(scope)

	// Load the pending row to verify ownership and get the conversationID.
	row, err := sq.GetPendingConfirmation(ctx, callID)
	if err != nil {
		return err // already wrapped with ErrConfirmationNotFound if not found
	}

	// Verify the callID belongs to the path conversation — the {id} segment must
	// not be decorative. Return not-found (not 403) so the response does not reveal
	// that the callID belongs to a different conversation.
	if row.ConversationID != convID {
		return fmt.Errorf("resolve: %w", ErrConfirmationNotFound)
	}

	// Atomic CAS UPDATE: SET status=@status WHERE ... AND status='pending'.
	// Returns ErrConfirmationAlreadyResolved when rowsAffected==0 (another concurrent
	// Resolve already transitioned this row). The pre-write read-then-check from the
	// previous implementation is dropped — the CAS is the single atomic winner.
	status := "denied"
	if approved {
		status = "approved"
	}
	if err := sq.UpdatePendingConfirmationStatus(ctx, callID, status); err != nil {
		return fmt.Errorf("resolve: update status: %w", err)
	}

	conv := row.ConversationID
	origin := ResolveOrigin()

	// Re-enter run asynchronously (same pattern as StartTurn).
	//nolint:gosec // Intentional: the resumed turn must outlive the request context.
	go func() {
		// Per-conversation serialisation: same lock as StartTurn so at most one
		// run goroutine executes per conversationID at a time. G2 blocks here
		// until G1's run returns; when G2 proceeds it re-lists persisted state,
		// sees G1's results already persisted, and the idempotency check in
		// processToolCalls (toolResultSet) skips already-done calls.
		entry := h.convLocks.acquire(conv)
		entry.mu.Lock()
		defer func() {
			entry.mu.Unlock()
			h.convLocks.release(conv)
		}()

		defer func() {
			if r := recover(); r != nil {
				h.logger.Error("harness: resume turn panicked", "conversationId", conv, "callId", callID, "panic", r)
				h.bus.Publish(principal.UserID, events.Event{
					Type: "turn.failed",
					Data: TurnFailedPayload{Code: "infrastructure"},
				})
			}
		}()

		if err := h.run(context.Background(), conv, origin, principal); err != nil {
			h.logger.Error("harness: resume turn failed", "conversationId", conv, "callId", callID, "err", err)
			h.bus.Publish(principal.UserID, events.Event{
				Type: "turn.failed",
				Data: TurnFailedPayload{Code: "infrastructure"},
			})
		}
	}()

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

// isToolResultUniqueViolation reports whether err is a UNIQUE constraint
// violation on the messages(conversation_id, tool_call_id) partial index. Used
// as a defense-in-depth check: if the per-conversation lock is somehow bypassed,
// treat a duplicate tool-result insert as "already done" rather than erroring
// the turn.
func isToolResultUniqueViolation(err error) bool {
	var sqliteErr sqlite3.Error
	return errors.As(err, &sqliteErr) && sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique
}
