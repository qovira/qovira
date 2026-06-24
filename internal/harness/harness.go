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

// NewDiscardLogger returns a *slog.Logger that discards all output below Error, for use in tests.
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
	ConversationID string `json:"conversationId"`
	MessageID      string `json:"messageId"`
	FinishReason   string `json:"finishReason"`
}

// ToolStartedPayload is the Data for a "tool.started" event, emitted before a
// tool call is executed. Risk is rendered as its string name (e.g. "read",
// "write", "external", "destructive"). ArgsSummary is compact JSON of the call
// arguments, truncated to a reasonable length.
type ToolStartedPayload struct {
	ConversationID string `json:"conversationId"`
	CallID         string `json:"callId"`
	Name           string `json:"name"`
	Risk           string `json:"risk"`
	ArgsSummary    string `json:"argsSummary"`
}

// ToolCompletedPayload is the Data for a "tool.completed" event, emitted after
// a tool call has been executed and its result persisted.
type ToolCompletedPayload struct {
	ConversationID string `json:"conversationId"`
	CallID         string `json:"callId"`
	Result         any    `json:"result"`
}

// ConfirmationExpiredPayload is the Data for a "confirmation.expired" event,
// emitted when a pending confirmation lapses past its ExpiresAt deadline. It is
// emitted on BOTH the lazy-check path (in Resolve) and the sweep path (in
// SweepExpiredConfirmations) so the UI chip can update immediately. Whether a
// model round fires depends on the case: fully-stale (all siblings expired) →
// no round, message abandoned; mixed (some sibling approved/denied) → run re-enters
// for the continue round so approved actions are narrated.
type ConfirmationExpiredPayload struct {
	// ConversationID is the conversation the expired confirmation belongs to.
	ConversationID string `json:"conversationId"`
	// CallID is the gateway tool call ID that was waiting for confirmation.
	CallID string `json:"callId"`
}

// ToolFailedPayload is the Data for a "tool.failed" event, emitted when a tool
// call returns a *capability.ToolError (a model-visible, self-correctable error).
// Error carries only the model-safe ToolError.Message — no internal detail.
type ToolFailedPayload struct {
	ConversationID string `json:"conversationId"`
	CallID         string `json:"callId"`
	Error          string `json:"error"`
}

// TurnFailedPayload is the Data for a "turn.failed" event, emitted when the
// turn aborts due to an infrastructure error (auth failure, upstream error,
// network failure, recovered panic, etc.). Code is a stable, generic class
// string — NOT the raw error text; the raw detail goes to the server log only.
type TurnFailedPayload struct {
	ConversationID string `json:"conversationId"`
	Code           string `json:"code"`
}

// ConfirmationRequiredPayload is the Data for a "confirmation.required" event,
// emitted when a Confirm-tier tool call is encountered and a pending_confirmations
// row has been created. The client should present the user with an approve/deny
// choice and POST to .../confirmations/{callId}.
type ConfirmationRequiredPayload struct {
	// ConversationID is the conversation the confirmation belongs to.
	ConversationID string `json:"conversationId"`
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
				h.logger.Error("harness: turn panicked", "conversationId", conv, "panic", sanitizePanic(r))
				h.bus.Publish(principal.UserID, events.Event{
					Type: "turn.failed",
					Data: TurnFailedPayload{ConversationID: conv, Code: "infrastructure"},
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
				Data: TurnFailedPayload{ConversationID: conv, Code: "infrastructure"},
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
		// Load the conversation history once per step iteration. All helpers that
		// read messages within this step use this slice, eliminating redundant
		// SQLCipher round-trips. When a message is persisted during the step, the
		// returned row (from RETURNING) is appended so downstream helpers in the same
		// iteration see exactly what a fresh ListMessages would return at that point.
		msgs, err := sq.ListMessages(ctx, conv)
		if err != nil {
			return fmt.Errorf("harness: list messages (step %d): %w", step, err)
		}

		// ── Resume path: process outstanding tool calls from the last assistant message ──
		// Before calling the gateway, check whether the last assistant message has
		// tool_calls with unresolved results. This handles the re-entry case from
		// Resolve: the conversation is in mid-round state (assistant message persisted
		// but tool calls not fully resolved). Process them idempotently first.
		pendingCalls, pendErr := outstandingToolCalls(msgs)
		if pendErr != nil {
			return fmt.Errorf("harness: check outstanding tool calls (step %d): %w", step, pendErr)
		}
		if len(pendingCalls) > 0 {
			// There are unresolved tool calls from a previous (persisted) round.
			// Process them idempotently without calling the gateway.
			suspended, procErr := h.processToolCalls(ctx, sq, msgs, conv, principal.UserID, pendingCalls, toolMap, scope, origin, step)
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
			if isTurnComplete(msgs) {
				return nil // turn already finished by a concurrent runner — no-op
			}

			// ── Normal path: call the gateway for a new model round ──────────────────
			//
			// runStep executes one model call (assemble → chat → stream → consume),
			// handling ErrContextLength at BOTH setup and stream levels with bounded
			// trim-and-retry inline.
			sr := h.runStep(ctx, sq, msgs, conv, principal.UserID, gwTools, profile, &trimLevel, step)
			if sr.termDone {
				return sr.termErr
			}
			if sr.err != nil {
				return sr.err
			}

			// Done with no tool calls: this is the final reply — persist and stop.
			if len(sr.calls) == 0 {
				assistantRow, err := persistAssistantMessage(ctx, sq, conv, string(sr.text), "", "stop")
				if err != nil {
					return fmt.Errorf("harness: persist final assistant message: %w", err)
				}
				h.bus.Publish(principal.UserID, events.Event{
					Type: "message.completed",
					Data: CompletedPayload{
						ConversationID: conv,
						MessageID:      assistantRow.ID,
						FinishReason:   "stop",
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
			assistantRow, err := persistAssistantMessage(ctx, sq, conv, string(sr.text), string(toolCallsJSON), "tool_calls")
			if err != nil {
				return fmt.Errorf("harness: persist assistant message with tool_calls (step %d): %w", step, err)
			}
			// Append the persisted assistant row so findAssistantMessageForCall (called
			// from insertPendingConfirmation inside processToolCalls) sees it in the
			// same step — preserving persist-then-read ordering without a re-query.
			msgs = append(msgs, insertRowToListRow(assistantRow))

			// Process tool calls using the idempotent, state-driven loop.
			suspended, procErr := h.processToolCalls(ctx, sq, msgs, conv, principal.UserID, sr.calls, toolMap, scope, origin, step)
			if procErr != nil {
				return procErr
			}
			if suspended {
				// Turn is suspended — waiting for user confirmation(s). The turn goroutine
				// ends here; no terminal event is emitted. Resume is via Resolve.
				return nil
			}
		}
		// Loop: next iteration will re-load messages at the top of the next step.
	}

	// Step cap reached: persist a graceful message and end the turn.
	const capMessage = "I wasn't able to finish that — I reached the maximum number of steps. Please try again."
	capRow, err := persistAssistantMessage(ctx, sq, conv, capMessage, "", "step_cap")
	if err != nil {
		return fmt.Errorf("harness: persist step-cap message: %w", err)
	}
	h.bus.Publish(principal.UserID, events.Event{
		Type: "message.completed",
		Data: CompletedPayload{
			ConversationID: conv,
			MessageID:      capRow.ID,
			FinishReason:   "step_cap",
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
// msgs is the conversation history loaded once by the caller at the top of the
// step iteration; runStep does not re-query the store. On context-length retries
// the same slice is re-used with a harder trimLevel — history does not change
// between retries.
//
// The returned stepResult carries exactly one meaningful outcome:
//   - text/calls populated, err nil, termDone false: consumed round; caller processes.
//   - err non-nil, termDone false: infra error; caller returns err.
//   - termDone true: context-length retries exhausted; graceful terminal already
//     emitted; caller returns termErr.
func (h *Harness) runStep(
	ctx context.Context,
	sq *store.ScopedQueries,
	msgs []db.ListMessagesRow,
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
		req, err := h.assembleChatRequest(msgs, gwTools, profile, *trimLevel)
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
				// Normalize empty/missing IDs before persistence so the idempotency
				// keys (resultIDs / doneIDs) and the partial-unique DB index engage.
				// Without this, a NULL-id result is never matched by the idempotency
				// check and the tool re-runs on every loop iteration until stepCap.
				if chunk.ToolCall.ID == "" {
					chunk.ToolCall.ID = generateID()
				}
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
			return stepResult{err: fmt.Errorf("harness: stream (step %d): %w", step, streamErr)}
		}

		// Stream consumed successfully.
		return stepResult{text: textAccum, calls: calls}
	}
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
	clRow, persistErr := persistAssistantMessage(ctx, sq, conv, msg, "", "context_length")
	if persistErr != nil {
		// Persist failure is infrastructure — propagate; StartTurn emits turn.failed.
		return true, fmt.Errorf("harness: persist context-length message: %w", persistErr)
	}
	// Emit message.completed as the sole terminal event. turn.failed must NOT be
	// emitted alongside it — clients treat the two as mutually exclusive.
	h.bus.Publish(userID, events.Event{
		Type: "message.completed",
		Data: CompletedPayload{
			ConversationID: conv,
			MessageID:      clRow.ID,
			FinishReason:   "context_length",
		},
	})
	return true, nil
}

// ── assembly ─────────────────────────────────────────────────────────────────

// assembleChatRequest builds a gateway.ChatRequest from the persisted message
// history. It:
//  1. Composes a per-turn system prompt (identity + user context + memory slot).
//  2. Round-trips each message in msgs (user/assistant/tool roles) to a gateway
//     message. msgs is the in-memory slice maintained by run() — no store query
//     is performed here.
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
	msgs []db.ListMessagesRow,
	gwTools []gateway.ToolSchema,
	profile db.User,
	trimLevel int,
) (gateway.ChatRequest, error) {
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
//
// msgs is the in-memory message slice maintained by run(). On the normal path it
// includes the just-persisted assistant row (appended before this call) so that
// findAssistantMessageForCall can locate it without a re-query.
func (h *Harness) processToolCalls(
	ctx context.Context,
	sq *store.ScopedQueries,
	msgs []db.ListMessagesRow,
	conv, userID string,
	calls []*gateway.ToolCall,
	toolMap map[string]capability.Tool,
	scope store.Scope,
	origin Origin,
	step int,
) (suspended bool, err error) {
	// Build a set of call IDs that already have a persisted tool-result message.
	// This is the idempotency check — re-entry after resume skips already-done calls.
	doneIDs := toolResultSet(msgs)

	allHandled := true
	for _, c := range calls {
		// Skip if already has a result (idempotent on re-entry).
		if doneIDs[c.ID] {
			continue
		}

		// Look up the tool once — reused for the policy gate, the unknown-tool path,
		// and execution below.
		tool, found := toolMap[c.Name]

		// Unknown tools have no risk tier; the !found path below handles them before
		// the policy switch, so pass a zero risk only when found is true.
		var dec Decision
		if found {
			dec = policy(tool.Risk, origin.Trust)
		}

		// Unknown tools: no risk tier; skips the policy gate. Emit tool.started and
		// persist a model-visible "unknown tool" error so the model can self-correct.
		if !found {
			// Emit tool.started for the unknown tool (it was an attempt, just hallucinated).
			h.bus.Publish(userID, events.Event{
				Type: "tool.started",
				Data: ToolStartedPayload{
					ConversationID: conv,
					CallID:         c.ID,
					Name:           c.Name,
					Risk:           "",
					ArgsSummary:    argsSummary(c.Arguments),
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
					if insertErr := h.insertPendingConfirmation(ctx, sq, msgs, conv, userID, c, tool); insertErr != nil {
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

				case "expired":
					// Persist a synthetic "expired" tool-result so the call is "done"
					// from the model's perspective (the OpenAI message sequence requires
					// a tool-result for every tool_call ID). outstandingToolCalls keys off
					// per-call tool-results; this synthetic result will appear in resultIDs
					// on the next iteration, keeping pending siblings correctly outstanding.
					if persistErr := h.persistExpiredResult(ctx, sq, conv, c.ID); persistErr != nil {
						return false, fmt.Errorf("harness: persist expired result for call %q: %w", c.ID, persistErr)
					}
					doneIDs[c.ID] = true

				default:
					// Unknown status: do not execute. Treat as still-pending.
					allHandled = false
				}
			}
		}
	}

	return !allHandled, nil
}

// ── Pure in-memory helpers (no store I/O) ─────────────────────────────────────

// insertRowToListRow converts a db.InsertMessageRow (returned by RETURNING on
// INSERT) to a db.ListMessagesRow so it can be appended to the in-memory slice
// that run() threads through helpers. Both types carry the same columns in the
// same order, so a direct conversion suffices — and if they ever diverge this
// stops compiling, which is a louder failure than a silently-partial copy.
func insertRowToListRow(r db.InsertMessageRow) db.ListMessagesRow {
	return db.ListMessagesRow(r)
}

// buildToolResultSet returns the set of tool call IDs that already have a
// persisted tool-result row in msgs. It is the shared sub-helper used by both
// outstandingToolCalls and toolResultSet — a single source of truth for the
// rule "a tool call is done when a role=tool row with its ID exists".
func buildToolResultSet(msgs []db.ListMessagesRow) map[string]bool {
	set := make(map[string]bool)
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID.Valid && m.ToolCallID.String != "" {
			set[m.ToolCallID.String] = true
		}
	}
	return set
}

// outstandingToolCalls inspects the in-memory message slice to determine whether
// the last assistant message has tool_calls entries that do not yet have a
// corresponding tool-result message. It returns the outstanding calls in original
// order. If there are no outstanding calls (either because the last message is not
// an assistant+tool_calls message, or all calls already have results), the
// returned slice is nil.
//
// The caller (run) loads msgs once per step iteration via sq.ListMessages; this
// function performs no store I/O.
func outstandingToolCalls(msgs []db.ListMessagesRow) ([]*gateway.ToolCall, error) {
	if len(msgs) == 0 {
		return nil, nil
	}

	// Build a set of tool call IDs that already have results.
	resultIDs := buildToolResultSet(msgs)

	// Find the last assistant message that has tool_calls.
	// Abandoned messages (Abandoned != 0) are only inert when ALL of their
	// confirm rows have been expired — i.e., when the message was abandoned by
	// the fully-stale path. In the mixed case (some approved/denied, others
	// expired), the message is NOT abandoned and siblings may still be pending.
	// We key off per-call tool-results, not the message-level abandoned flag, so
	// that an expired call's synthetic result marks it "done" while pending
	// siblings remain outstanding.
	for _, m := range slices.Backward(msgs) {
		if m.Role != "assistant" || !m.ToolCalls.Valid || m.ToolCalls.String == "" {
			continue
		}
		// A message-level abandoned flag means every confirm row for this message
		// expired (fully-stale case). isTurnComplete treats this as terminal —
		// do not re-enter run for it. If outstandingToolCalls were to return
		// outstanding calls here, the caller would try to re-process them, which
		// is wrong because they will all show status="expired" (and have synthetic
		// tool-results) or the message would not have been abandoned.
		if m.Abandoned != 0 {
			return nil, nil
		}
		var tcs []gateway.ToolCall
		if jsonErr := json.Unmarshal([]byte(m.ToolCalls.String), &tcs); jsonErr != nil {
			return nil, fmt.Errorf("unmarshal tool_calls for message %s: %w", m.ID, jsonErr)
		}
		// Collect calls that do NOT yet have a persisted tool-result. An expired
		// call's synthetic tool-result is in resultIDs, so it is treated as "done"
		// here — its sibling pending calls remain outstanding as expected.
		var outstanding []*gateway.ToolCall
		for j := range tcs {
			if !resultIDs[tcs[j].ID] {
				tc := tcs[j] // copy so we can take address
				outstanding = append(outstanding, &tc)
			}
		}
		if len(outstanding) == 0 {
			// All calls for this assistant message have results — not a suspended state.
			return nil, nil
		}
		return outstanding, nil
	}
	return nil, nil
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
func isTurnComplete(msgs []db.ListMessagesRow) bool {
	if len(msgs) == 0 {
		return false
	}
	last := msgs[len(msgs)-1]
	if last.Role != "assistant" {
		return false
	}
	// An abandoned assistant message is terminal: expiry ended the turn without a
	// model round. Treat the conversation as complete so run never re-enters.
	if last.Abandoned != 0 {
		return true
	}
	// An assistant message with tool_calls is not yet complete — tool results are
	// still outstanding (the resume path above handles this before we reach here,
	// so in practice this branch is not reached, but guard it for correctness).
	if last.ToolCalls.Valid && last.ToolCalls.String != "" {
		return false
	}
	// Last message is an assistant final reply (no tool_calls) — turn is done.
	return true
}

// toolResultSet builds a set of tool call IDs that already have a persisted
// tool-result message in msgs. This drives the idempotency check in
// processToolCalls. It delegates to buildToolResultSet so the boundary rule
// lives in one place.
func toolResultSet(msgs []db.ListMessagesRow) map[string]bool {
	return buildToolResultSet(msgs)
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
			ConversationID: conv,
			CallID:         call.ID,
			Name:           call.Name,
			Risk:           risk,
			ArgsSummary:    argsSummary(call.Arguments),
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
			ConversationID: conv,
			CallID:         call.ID,
			Result:         result,
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
//
// msgs is the in-memory slice threaded from run(); it already includes the
// just-persisted assistant row so findAssistantMessageForCall can locate it
// without a re-query.
func (h *Harness) insertPendingConfirmation(
	ctx context.Context,
	sq *store.ScopedQueries,
	msgs []db.ListMessagesRow,
	conv, userID string,
	call *gateway.ToolCall,
	tool capability.Tool,
) error {
	// Look up the assistant message ID that holds this tool call. It is the most
	// recent assistant message in this conversation that has tool_calls.
	msgID, err := findAssistantMessageForCall(msgs, call.ID)
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
			ConversationID: conv,
			CallID:         call.ID,
			Name:           call.Name,
			Risk:           risk,
			Args:           call.Arguments,
			ExpiresAt:      expiresAt,
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

// marshalExpiredResultJSON returns the canonical JSON payload for a synthetic
// expired tool-result. The payload is model-safe: {"error":"…","code":"confirmation_expired"}.
// It is extracted here so both the lazy (persistExpiredResult) and sweep
// (persistExpiredResultByUserID) paths share a single source of truth — previously
// duplicating the const and json.Marshal call in two places let the bug hide.
func marshalExpiredResultJSON(callID string) (string, error) {
	const expiredMsg = "This confirmation expired; the action was not performed."
	b, err := json.Marshal(map[string]string{
		"error": expiredMsg,
		"code":  "confirmation_expired",
	})
	if err != nil {
		return "", fmt.Errorf("marshal expired result for call %s: %w", callID, err)
	}
	return string(b), nil
}

// persistExpiredResult persists a synthetic model-visible tool-result message for an
// expired confirmation. The content signals to the model that the action was not
// performed due to expiry. This keeps the OpenAI message sequence valid: a tool-result
// is required for every tool_call ID in the assistant message, even when the call
// expired without executing.
//
// On a UNIQUE constraint violation (duplicate from a concurrent path), returns nil
// so idempotency is preserved — the CAS already guarantees exactly one winner.
//
// Note on fatality: processToolCalls treats a persistExpiredResult failure as fatal
// (returns error so the resume loop aborts). The callers here (expireCallCore and
// expireCallAndMaybeAbandon*) only log and continue because missing a synthetic
// tool-result is tolerable housekeeping — the row already transitioned to "expired"
// via the CAS, so the UI is consistent even if the model message sequence has a gap.
func (h *Harness) persistExpiredResult(ctx context.Context, sq *store.ScopedQueries, conv, callID string) error {
	resultJSON, err := marshalExpiredResultJSON(callID)
	if err != nil {
		return err
	}
	if _, err := sq.InsertMessage(ctx, store.InsertMessageParams{
		ID:             generateID(),
		ConversationID: conv,
		Role:           "tool",
		Content:        resultJSON,
		ToolCallID:     callID,
	}); err != nil {
		if isToolResultUniqueViolation(err) {
			return nil // already persisted by a concurrent path
		}
		return fmt.Errorf("persist expired result for call %s: %w", callID, err)
	}
	return nil
}

// persistExpiredResultByUserID is the sweep-path analogue of persistExpiredResult.
// It uses the system-scoped ScopedQueries and the row's own userID to bypass the
// scope check — same pattern as MarkMessageAbandonedByUserID.
//
// Note on fatality: same rationale as persistExpiredResult — the sweep caller logs
// and continues rather than aborting, because housekeeping tolerates a missing row.
func (h *Harness) persistExpiredResultByUserID(ctx context.Context, sysSQ *store.ScopedQueries, conv, userID, callID string) error {
	resultJSON, err := marshalExpiredResultJSON(callID)
	if err != nil {
		return err
	}
	if err := sysSQ.InsertMessageByUserID(ctx, generateID(), conv, userID, callID, resultJSON); err != nil {
		if isToolResultUniqueViolation(err) {
			return nil
		}
		return fmt.Errorf("persist expired result (sweep) for call %s: %w", callID, err)
	}
	return nil
}

// findAssistantMessageForCall finds the ID of the most recent assistant message
// in msgs that contains the given tool call ID in its tool_calls array. This is
// the parent message for the pending_confirmations row.
//
// msgs is the in-memory slice maintained by run(); on the normal path it already
// includes the just-persisted assistant row (appended before processToolCalls is
// called), so no store query is needed.
func findAssistantMessageForCall(msgs []db.ListMessagesRow, callID string) (string, error) {
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

// ErrConfirmationExpired is returned by Resolve when the pending row's ExpiresAt
// is in the past. Callers map this to HTTP 409 with code "confirmation_expired".
// Expiry is asymmetric with deny: in the fully-stale case (all siblings expired)
// no model round is spawned and the message is abandoned. In the mixed case (at
// least one sibling was approved/denied by the user), run re-enters so the continue
// round narrates the approved sibling's result.
var ErrConfirmationExpired = store.ErrConfirmationExpired

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
//   - ErrConfirmationAlreadyResolved: the row is already resolved (another concurrent
//     Resolve won the CAS) → handler returns 409.
//   - ErrConfirmationExpired: the row is pending but past ExpiresAt → handler returns
//     409 with code "confirmation_expired". The row is transitioned to "expired" and
//     confirmation.expired is emitted. Fully-stale case (all siblings expired): the
//     assistant message is abandoned, no model round fires. Mixed case (at least one
//     sibling was approved/denied): run re-enters so the continue round narrates the
//     approved sibling's result.
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

	// Atomic CAS UPDATE: SET status=@status WHERE ... AND status='pending' AND NOT (expires_at < @now).
	// This is the lazy expiry check — an expired row is NOT approved/denied by this CAS.
	// If it finds 0 rows, the store distinguishes:
	//   - not-found → ErrConfirmationNotFound
	//   - already resolved (non-pending) → ErrConfirmationAlreadyResolved
	//   - pending but past ExpiresAt → ErrConfirmationExpired (we handle this below)
	now := h.now().UTC().Format(time.RFC3339)
	status := "denied"
	if approved {
		status = "approved"
	}
	conv := row.ConversationID
	casErr := sq.UpdatePendingConfirmationStatusIfCurrent(ctx, callID, status, now)
	if casErr != nil {
		if errors.Is(casErr, ErrConfirmationExpired) {
			// The row is pending but lapsed. Run the expiry CAS atomically:
			// SET status='expired' WHERE id=? AND user_id=? AND status='pending'.
			// If the sweep won between our read and this CAS, n==0 and we skip to return.
			expErr := sq.MarkConfirmationExpired(ctx, callID)
			if expErr == nil {
				// We won the expiry CAS — persist the synthetic expired tool-result,
				// emit confirmation.expired, and either abandon the message (fully-stale)
				// or re-enter run (mixed case) so the continue round fires.
				if aErr := h.expireCallAndMaybeAbandon(ctx, sq, conv, row.MessageID, principal.UserID, callID, principal); aErr != nil {
					// Non-fatal: log and return ErrConfirmationExpired anyway.
					h.logger.Error("harness: expire call after lazy expiry CAS", "callId", callID, "err", aErr)
				}
			}
			// Whether we won or the sweep won, the caller gets ErrConfirmationExpired → 409.
			return fmt.Errorf("resolve: %w", ErrConfirmationExpired)
		}
		return fmt.Errorf("resolve: update status: %w", casErr)
	}
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
				h.logger.Error("harness: resume turn panicked", "conversationId", conv, "callId", callID, "panic", sanitizePanic(r))
				h.bus.Publish(principal.UserID, events.Event{
					Type: "turn.failed",
					Data: TurnFailedPayload{ConversationID: conv, Code: "infrastructure"},
				})
			}
		}()

		if err := h.run(context.Background(), conv, origin, principal); err != nil {
			h.logger.Error("harness: resume turn failed", "conversationId", conv, "callId", callID, "err", err)
			h.bus.Publish(principal.UserID, events.Event{
				Type: "turn.failed",
				Data: TurnFailedPayload{ConversationID: conv, Code: "infrastructure"},
			})
		}
	}()

	return nil
}

// expireCallCore is the shared implementation for both the lazy (Resolve) and
// sweep (SweepExpiredConfirmations) expiry paths. It:
//
//  1. Persists a synthetic expired tool-result via persistFn (best-effort; logs on fail).
//  2. Emits confirmation.expired on the user bus.
//  3. Counts non-expired siblings via countNonExpiredFn:
//     - If count == 0 (fully stale — all confirms for this message expired):
//     marks the assistant message abandoned via abandonFn and returns (abandoned=true).
//     - If count > 0 (mixed case — at least one sibling is still pending or already
//     approved/denied): returns (abandoned=false) so the caller can re-enter run.
//
// The (abandoned, error) return is consumed by the caller to decide whether to
// spawn a resume goroutine. The fully-stale/abandoned case MUST NOT re-enter run
// (no user action deserves a model round). The mixed/non-abandoned case MUST re-enter
// run so the continue round narrates B's approved action — the "no model round on
// expiry" rule applies only to fully-stale turns.
func (h *Harness) expireCallCore(
	conv, messageID, userID, callID string,
	persistFn func() error,
	countNonExpiredFn func() (int64, error),
	abandonFn func() error,
) (abandoned bool, err error) {
	// Best-effort persist: if it fails (e.g. duplicate from a concurrent path), log
	// and continue — the CAS already guarantees exactly one winner for expiry.
	if persistErr := persistFn(); persistErr != nil {
		h.logger.Error("harness: persist expired tool-result", "callId", callID, "conv", conv, "err", persistErr)
	}

	// Emit confirmation.expired so the UI chip updates immediately.
	h.bus.Publish(userID, events.Event{
		Type: "confirmation.expired",
		Data: ConfirmationExpiredPayload{ConversationID: conv, CallID: callID},
	})

	// Gate message abandonment: count how many of this message's confirm rows are
	// NOT expired. If none remain (count == 0), all confirms for this message have
	// expired without user action — abandon the message so isTurnComplete treats it
	// as terminal (no model round for "nothing to do").
	nonExpiredCount, countErr := countNonExpiredFn()
	if countErr != nil {
		// Non-fatal; log and skip abandon — a stale message is better than an error.
		// Treat as non-abandoned so any pending sibling re-entry still proceeds.
		h.logger.Error("harness: count non-expired confirmations", "messageId", messageID, "err", countErr)
		return false, nil
	}
	if nonExpiredCount > 0 {
		// Mixed case: at least one sibling is still pending or already resolved by the user.
		// Do NOT abandon — the turn is resumable; caller re-enters run.
		return false, nil
	}
	// Fully stale: all confirms for this message expired. Abandon the message.
	if aErr := abandonFn(); aErr != nil {
		return false, fmt.Errorf("mark message abandoned (fully stale): %w", aErr)
	}
	return true, nil
}

// expireCallAndMaybeAbandon handles a single call expiry on the lazy path
// (called from Resolve after winning the MarkConfirmationExpired CAS). It delegates
// to expireCallCore with the user-scoped store functions, then:
//   - Fully-stale case (abandoned=true): does NOT re-enter run (no model round for "nothing to do").
//   - Mixed case (abandoned=false): re-enters run in a new goroutine so the continue
//     round narrates the approved sibling(s) — mirrors the approve/deny resume path.
func (h *Harness) expireCallAndMaybeAbandon(
	ctx context.Context,
	sq *store.ScopedQueries,
	conv, messageID, userID, callID string,
	principal store.Principal,
) error {
	abandoned, err := h.expireCallCore(
		conv, messageID, userID, callID,
		func() error { return h.persistExpiredResult(ctx, sq, conv, callID) },
		func() (int64, error) { return sq.CountNonExpiredConfirmationsByMessageID(ctx, messageID) },
		func() error { return sq.MarkMessageAbandoned(ctx, messageID) },
	)
	if err != nil {
		return err
	}
	if abandoned {
		// Fully stale — no model round. The "no model round on expiry" rule covers
		// only this case: all confirms expired without any user action; the turn is dead.
		return nil
	}
	// Mixed case: at least one sibling was approved or denied (legitimate user action).
	// Re-enter run so the continue round narrates the executed result. Re-entry is
	// idempotent: if other siblings are still pending, run re-suspends harmlessly.
	origin := ResolveOrigin()
	//nolint:gosec // Intentional: the resumed turn must outlive the request context.
	go func() {
		entry := h.convLocks.acquire(conv)
		entry.mu.Lock()
		defer func() {
			entry.mu.Unlock()
			h.convLocks.release(conv)
		}()
		defer func() {
			if r := recover(); r != nil {
				h.logger.Error("harness: expiry resume panicked", "conversationId", conv, "callId", callID, "panic", sanitizePanic(r))
				h.bus.Publish(principal.UserID, events.Event{
					Type: "turn.failed",
					Data: TurnFailedPayload{ConversationID: conv, Code: "infrastructure"},
				})
			}
		}()
		if runErr := h.run(context.Background(), conv, origin, principal); runErr != nil {
			h.logger.Error("harness: expiry resume failed", "conversationId", conv, "callId", callID, "err", runErr)
			h.bus.Publish(principal.UserID, events.Event{
				Type: "turn.failed",
				Data: TurnFailedPayload{ConversationID: conv, Code: "infrastructure"},
			})
		}
	}()
	return nil
}

// expireCallAndMaybeAbandonByUserID is the sweep-path analogue of
// expireCallAndMaybeAbandon. It uses the row's own user_id (not the bound scope)
// because the sweep operates as system housekeeping across all users. The logic is
// identical: persist a synthetic expired tool-result, emit confirmation.expired,
// and abandon the assistant message only when all siblings are also expired.
//
// The re-entry goroutine (non-abandoned case) is spawned by the caller
// (SweepExpiredConfirmations), not here, to allow deduplication across multiple
// rows that belong to the same conversation — spawn at most once per conv per sweep.
func (h *Harness) expireCallAndMaybeAbandonByUserID(
	ctx context.Context,
	sysSQ *store.ScopedQueries,
	conv, messageID, userID, callID string,
) (abandoned bool, err error) {
	return h.expireCallCore(
		conv, messageID, userID, callID,
		func() error { return h.persistExpiredResultByUserID(ctx, sysSQ, conv, userID, callID) },
		func() (int64, error) {
			return sysSQ.CountNonExpiredConfirmationsByMessageIDForUser(ctx, messageID, userID)
		},
		func() error {
			aErr := sysSQ.MarkMessageAbandonedByUserID(ctx, messageID, userID)
			if aErr != nil {
				h.logger.Error("harness: abandon assistant message (sweep)", "callId", callID, "messageId", messageID, "err", aErr)
			}
			return nil // sweep abandonment errors are non-fatal
		},
	)
}

// SweepExpiredConfirmations marks all lapsed pending_confirmations rows as expired,
// abandons their assistant messages, and emits confirmation.expired on each row's
// user bus. Returns the count of rows swept.
//
// This method is designed to be registered as a periodic job by the Scheduler
// (internal/scheduler, project #6 — not yet built). Leave this as a directly-callable
// method until the scheduler is wired. Shape: (ctx context.Context) (int, error)
// so the scheduler can wrap it trivially as a func(ctx) error.
//
// The lapsed-row query runs across all users (SYSTEM-HOUSEKEEPING, allow-unscoped).
// Each expired row is processed atomically via a per-row CAS so a concurrent Resolve
// racing the sweep on the same row has exactly one winner.
//
// After processing all rows, the sweep collects the distinct conversation IDs that
// are NON-abandoned (mixed case: at least one sibling was approved/denied by the user)
// and spawns a resume goroutine ONCE PER CONVERSATION so the continue round fires for
// each affected turn. Deduplication avoids goroutine spam when multiple rows from the
// same conversation appear in a single sweep batch.
func (h *Harness) SweepExpiredConfirmations(ctx context.Context) (int, error) {
	// Use a system-scoped ScopedQueries for the cross-user SELECT.
	// The per-row CAS and message-abandon use the row's own user_id.
	sysSQ := h.store.ForUser(store.SystemScope())

	now := h.now().UTC().Format(time.RFC3339)
	lapsed, err := sysSQ.ListLapsedConfirmations(ctx, now)
	if err != nil {
		return 0, fmt.Errorf("sweep: list lapsed confirmations: %w", err)
	}

	// nonAbandonedConvs accumulates (convID → userID) for conversations that need a
	// resume goroutine after the sweep. Using a map deduplicates multiple rows from
	// the same conversation so we spawn at most one goroutine per conversation.
	type convResume struct{ userID string }
	nonAbandonedConvs := make(map[string]convResume)

	swept := 0
	for _, row := range lapsed {
		// Per-row CAS: only the first winner (this sweep vs a concurrent Resolve) transitions the row.
		n, casErr := sysSQ.MarkConfirmationExpiredByUserID(ctx, row.ID, row.UserID)
		if casErr != nil {
			h.logger.Error("sweep: mark confirmation expired", "callId", row.ID, "userID", row.UserID, "err", casErr)
			continue
		}
		if n == 0 {
			// Another path (concurrent Resolve or sweep iteration) already won the CAS — skip.
			continue
		}

		// We won the CAS. Persist the synthetic expired tool-result, emit
		// confirmation.expired, and (only if all siblings also expired) abandon the
		// assistant message so isTurnComplete treats it as terminal.
		abandoned, aErr := h.expireCallAndMaybeAbandonByUserID(ctx, sysSQ, row.ConversationID, row.MessageID, row.UserID, row.ID)
		if aErr != nil {
			h.logger.Error("sweep: expire call and maybe abandon", "callId", row.ID, "messageId", row.MessageID, "err", aErr)
			// Non-fatal: still count the sweep.
		}

		swept++

		// Track non-abandoned conversations for post-sweep re-entry. The fully-stale
		// (abandoned) case must NOT re-enter run — no user action deserves a model round.
		if !abandoned {
			nonAbandonedConvs[row.ConversationID] = convResume{userID: row.UserID}
		}
	}

	// Spawn a resume goroutine for each non-abandoned conversation so the continue
	// round fires and narrates any approved sibling actions.
	// Per-conversation lock makes concurrent re-entries safe; dedup above limits goroutines.
	origin := ResolveOrigin()
	for conv, cr := range nonAbandonedConvs {
		principal := store.Principal{UserID: cr.userID}
		//nolint:gosec // Intentional: the resumed turn must outlive the sweep context.
		go func() {
			entry := h.convLocks.acquire(conv)
			entry.mu.Lock()
			defer func() {
				entry.mu.Unlock()
				h.convLocks.release(conv)
			}()
			defer func() {
				if r := recover(); r != nil {
					h.logger.Error("harness: sweep expiry resume panicked", "conversationId", conv, "panic", sanitizePanic(r))
					h.bus.Publish(principal.UserID, events.Event{
						Type: "turn.failed",
						Data: TurnFailedPayload{ConversationID: conv, Code: "infrastructure"},
					})
				}
			}()
			if runErr := h.run(context.Background(), conv, origin, principal); runErr != nil {
				h.logger.Error("harness: sweep expiry resume failed", "conversationId", conv, "err", runErr)
				h.bus.Publish(principal.UserID, events.Event{
					Type: "turn.failed",
					Data: TurnFailedPayload{ConversationID: conv, Code: "infrastructure"},
				})
			}
		}()
	}

	return swept, nil
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
			ConversationID: conv,
			CallID:         callID,
			Error:          toolErr.Message,
		},
	})

	// Return nil so the loop continues — the model will self-correct.
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// persistAssistantMessage inserts the assistant reply into the messages table
// and returns the full persisted row (including server-generated created_at and
// abandoned flag). toolCallsJSON is the JSON array string for the tool_calls
// column, or "" if the message has no tool calls. finishReason is the value to
// record in the finish_reason column: "tool_calls" for a tool-call round, "stop"
// for a final text reply, or another value for synthetic terminal messages (e.g.
// "step_cap", "context_length").
//
// Returning the full row allows callers to append it to the in-memory message
// slice (via insertRowToListRow) so downstream helpers in the same iteration see
// the new row without a re-query, preserving persist-then-read ordering.
func persistAssistantMessage(
	ctx context.Context,
	sq *store.ScopedQueries,
	conv, content, toolCallsJSON, finishReason string,
) (db.InsertMessageRow, error) {
	row, err := sq.InsertMessage(ctx, store.InsertMessageParams{
		ID:             generateID(),
		ConversationID: conv,
		Role:           "assistant",
		Content:        content,
		ToolCalls:      toolCallsJSON,
		FinishReason:   finishReason,
	})
	return row, err
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

// truncateOnRuneBoundary returns s capped at limit bytes, backing up to the
// start of a UTF-8 rune so a multi-byte rune is never split (a raw byte cut can
// produce ill-formed UTF-8), and appending "…" when truncation occurs. Strings
// already within the cap are returned unchanged.
func truncateOnRuneBoundary(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	// Back up from the byte limit until we land on the start of a rune.
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// argsSummary returns a compact JSON summary of the given raw arguments, truncated
// to maxArgsSummaryBytes on a valid UTF-8 rune boundary. Truncating at a raw byte
// offset can split a multi-byte rune and produce ill-formed UTF-8, which would
// corrupt the JSON payload sent to the UI via "tool.started".
func argsSummary(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	return truncateOnRuneBoundary(string(raw), maxArgsSummaryBytes)
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

// maxPanicMsgBytes is the maximum number of bytes from a panic value's string
// representation that are logged. Mirrors the maxArgsSummaryBytes cap used for
// tool arguments, preventing unbounded sensitive data from appearing in logs.
const maxPanicMsgBytes = 256

// sanitizePanic returns a bounded, type-tagged string representation of a
// recovered panic value. It logs only the dynamic type and a truncated message
// string (capped at maxPanicMsgBytes), so sensitive tool arguments or upstream
// error strings embedded in the panic value cannot reach the log sink unbounded.
func sanitizePanic(r any) string {
	msg := truncateOnRuneBoundary(fmt.Sprintf("%v", r), maxPanicMsgBytes)
	return fmt.Sprintf("%T: %s", r, msg)
}
