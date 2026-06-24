package harness_test

// harness_errors_test.go — integration tests for error classification behavior. These test the observable
// outcomes (events, persisted data) for each fault class. Satisfies AC-1, AC-2, AC-4, and AC-5 from the
// error-classification issue.

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/capability"
	"github.com/qovira/qovira/internal/events"
	"github.com/qovira/qovira/internal/gateway"
	"github.com/qovira/qovira/internal/harness"
	"github.com/qovira/qovira/internal/id"
	"github.com/qovira/qovira/internal/store"
)

// ── helpers for error classification tests ────────────────────────────────────

// waitForTurnFailed blocks until a "turn.failed" event is present on the bus, or the timeout expires. Returns the
// full event snapshot.
func waitForTurnFailed(t *testing.T, b *fakeBus, timeout time.Duration) []fakeEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		evs := b.snapshot()
		for _, e := range evs {
			if e.event.Type == "turn.failed" {
				return evs
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	return b.snapshot()
}

// startTurnForErrors fires a turn by inserting a user message and calling StartTurn. Unlike startTurnAndWait it
// does not wait for message.completed because error paths may never emit that event.
func startTurnForErrors(
	t *testing.T,
	h *harness.Harness,
	s *store.Store,
	p store.Principal,
	convID string,
) {
	t.Helper()
	scope := store.UserScope(p)
	sq := s.ForUser(scope)

	if err := sq.UpsertConversation(context.Background(), convID); err != nil {
		t.Fatalf("UpsertConversation: %v", err)
	}
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             id.New(),
		ConversationID: convID,
		Role:           "user",
		Content:        "test message",
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	origin := harness.Origin{Channel: "web", Trust: harness.Trusted}
	if err := h.StartTurn(context.Background(), convID, harness.InboundMessage{Content: "test message"}, origin, p); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
}

// makeToolErrorTool builds a tool whose Execute always returns a *capability.ToolError.
func makeToolErrorTool(name, code, message string) capability.Tool {
	type args struct{}
	return capability.NewTool(
		name, "always fails with tool error",
		[]byte(`{"type":"object","properties":{}}`),
		capability.RiskRead,
		func(_ context.Context, _ store.Scope, _ args) (capability.Result, error) {
			return nil, &capability.ToolError{Code: code, Message: message}
		},
	)
}

// makePanicChatter returns a Chatter that panics when Chat is called.
func makePanicChatter(panicVal any) harness.Chatter {
	return &panicChatter{panicVal: panicVal}
}

type panicChatter struct {
	panicVal any
}

func (p *panicChatter) Chat(_ context.Context, _ gateway.ChatRequest) (iter.Seq2[gateway.Chunk, error], error) {
	panic(p.panicVal)
}

// errorChatter returns a Chatter that returns the given error from Chat setup (not stream).
type errorChatter struct {
	err error
}

func (e *errorChatter) Chat(_ context.Context, _ gateway.ChatRequest) (iter.Seq2[gateway.Chunk, error], error) {
	return nil, e.err
}

// streamErrorChatter returns a Chatter that delivers the given error mid-stream (after Done=false chunks).
type streamErrorChatter struct {
	mu     sync.Mutex
	rounds []streamErrorRound
	pos    int
}

type streamErrorRound struct {
	chunks []gateway.Chunk
	err    error // emitted after chunks via yield(Chunk{}, err)
}

func (s *streamErrorChatter) Chat(_ context.Context, _ gateway.ChatRequest) (iter.Seq2[gateway.Chunk, error], error) {
	s.mu.Lock()
	round := s.rounds[s.pos%len(s.rounds)]
	s.pos++
	s.mu.Unlock()

	seq := func(yield func(gateway.Chunk, error) bool) {
		for _, c := range round.chunks {
			if !yield(c, nil) {
				return
			}
		}
		if round.err != nil {
			yield(gateway.Chunk{}, round.err)
		}
	}
	return seq, nil
}

// ── AC-1: ToolError continues the loop ───────────────────────────────────────

// TestErrorClass_ToolError_ContinuesLoop verifies that a tool returning *capability.ToolError:
//   - Emits tool.failed with the model-safe message.
//   - Persists the tool result with the model-safe message as content.
//   - Continues the loop so a second round runs and message.completed is emitted.
func TestErrorClass_ToolError_ContinuesLoop(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	const (
		toolName       = "failing_tool"
		errCode        = "bad_args"
		errMessage     = "the 'name' argument is required"
		internalDetail = "db.query: pq: null violation on column 'name'"
	)

	reg := capability.NewRegistry()
	// A tool that returns a ToolError wrapping no internal detail.
	failingTool := makeToolErrorTool(toolName, errCode, errMessage)
	if err := reg.Add(fakeSource{tools: []capability.Tool{failingTool}}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	const callID = "call_toolerr1"
	round1 := []gateway.Chunk{
		toolCallChunk(callID, toolName, `{}`),
		{Done: true},
	}
	round2 := []gateway.Chunk{
		{TextDelta: "I see the issue, let me try again"},
		{Done: true},
	}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1, round2}}
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	startTurnForErrors(t, h, s, p, convID)

	// Wait for message.completed (loop continued to round 2).
	evs := waitForCompleted(t, bus, 1, 5*time.Second)

	// --- Assert: tool.failed was emitted ---
	var toolFailedEvts []harness.ToolFailedPayload
	for _, e := range evs {
		if e.event.Type == "tool.failed" {
			p, ok := e.event.Data.(harness.ToolFailedPayload)
			if !ok {
				t.Errorf("tool.failed Data type = %T, want harness.ToolFailedPayload", e.event.Data)
				continue
			}
			toolFailedEvts = append(toolFailedEvts, p)
		}
	}
	if len(toolFailedEvts) == 0 {
		t.Fatal("no tool.failed event emitted")
	}

	// AC-4: tool.failed carries ONLY the model-safe message — no internal detail.
	failedPayload := toolFailedEvts[0]
	if failedPayload.CallID != callID {
		t.Errorf("tool.failed callId = %q, want %q", failedPayload.CallID, callID)
	}
	if failedPayload.Error != errMessage {
		t.Errorf("tool.failed error = %q, want %q (model-safe message)", failedPayload.Error, errMessage)
	}
	if strings.Contains(failedPayload.Error, internalDetail) {
		t.Errorf("tool.failed error leaks internal detail: %q", failedPayload.Error)
	}

	// --- Assert: message.completed was emitted (loop continued) ---
	var completedCount int
	for _, e := range evs {
		if e.event.Type == "message.completed" {
			completedCount++
		}
	}
	if completedCount != 1 {
		t.Errorf("message.completed count = %d, want 1 (loop should continue after ToolError)", completedCount)
	}

	// --- Assert: tool result was persisted with model-safe message ---
	scope := store.UserScope(p)
	sq := s.ForUser(scope)
	msgs, err := sq.ListMessages(context.Background(), convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var toolResultContent string
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID.Valid && m.ToolCallID.String == callID {
			toolResultContent = m.Content
		}
	}
	if toolResultContent == "" {
		t.Fatal("tool-result message not persisted")
	}
	if !strings.Contains(toolResultContent, errMessage) {
		t.Errorf("tool-result content = %q, want it to contain %q", toolResultContent, errMessage)
	}
	if strings.Contains(toolResultContent, internalDetail) {
		t.Errorf("tool-result content leaks internal detail: %q", toolResultContent)
	}
}

// ── AC-2: infrastructure error aborts the turn ───────────────────────────────

// TestErrorClass_InfraError_AbortsWithTurnFailed verifies that a generic infrastructure error from the gateway:
//   - Emits turn.failed with a stable code (no raw error text).
//   - Does NOT emit message.completed.
//   - The raw error is NOT in any persisted message.
func TestErrorClass_InfraError_GatewaySetup_AbortsWithTurnFailed(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	infraErr := errors.New("tcp: connection refused (INTERNAL DB DETAIL)")
	gw := &errorChatter{err: infraErr}

	reg := capability.NewRegistry()
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	startTurnForErrors(t, h, s, p, convID)

	evs := waitForTurnFailed(t, bus, 5*time.Second)

	// Assert: turn.failed emitted.
	var turnFailedEvts []harness.TurnFailedPayload
	for _, e := range evs {
		if e.event.Type == "turn.failed" {
			p, ok := e.event.Data.(harness.TurnFailedPayload)
			if !ok {
				t.Errorf("turn.failed Data type = %T, want harness.TurnFailedPayload", e.event.Data)
				continue
			}
			turnFailedEvts = append(turnFailedEvts, p)
		}
	}
	if len(turnFailedEvts) == 0 {
		t.Fatal("no turn.failed event emitted for infra error")
	}

	// The code is a stable class string, not the raw error text.
	code := turnFailedEvts[0].Code
	if code == "" {
		t.Error("turn.failed code is empty")
	}
	if strings.Contains(code, "INTERNAL DB DETAIL") {
		t.Errorf("turn.failed code leaks internal error text: %q", code)
	}
	if strings.Contains(code, "tcp:") {
		t.Errorf("turn.failed code leaks raw error: %q", code)
	}

	// message.completed must NOT be emitted.
	for _, e := range evs {
		if e.event.Type == "message.completed" {
			t.Error("message.completed emitted after infra error — must not be")
		}
	}
}

// TestErrorClass_InfraError_StreamError_AbortsWithTurnFailed verifies that a generic infra error delivered
// mid-stream also aborts the turn.
func TestErrorClass_InfraError_StreamError_AbortsWithTurnFailed(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	infraErr := errors.New("stream EOF: INTERNAL STREAM DETAIL")
	gw := &streamErrorChatter{
		rounds: []streamErrorRound{
			{chunks: []gateway.Chunk{{TextDelta: "partial"}}, err: infraErr},
		},
	}

	reg := capability.NewRegistry()
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	startTurnForErrors(t, h, s, p, convID)

	evs := waitForTurnFailed(t, bus, 5*time.Second)

	var hasTurnFailed bool
	for _, e := range evs {
		if e.event.Type == "turn.failed" {
			hasTurnFailed = true
			p, ok := e.event.Data.(harness.TurnFailedPayload)
			if ok && strings.Contains(p.Code, "INTERNAL STREAM DETAIL") {
				t.Errorf("turn.failed code leaks internal stream detail: %q", p.Code)
			}
		}
		if e.event.Type == "message.completed" {
			t.Error("message.completed emitted after stream infra error — must not be")
		}
	}
	if !hasTurnFailed {
		t.Error("no turn.failed event emitted for stream infra error")
	}
}

// TestErrorClass_GatewayErrAuth_AbortsWithTurnFailed verifies that gateway ErrAuth (a typed gateway error) causes
// turn.failed, not a loop.
func TestErrorClass_GatewayErrAuth_AbortsWithTurnFailed(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	gw := &errorChatter{err: gateway.ErrAuth}
	reg := capability.NewRegistry()
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	startTurnForErrors(t, h, s, p, convID)

	evs := waitForTurnFailed(t, bus, 5*time.Second)

	var hasTurnFailed bool
	for _, e := range evs {
		if e.event.Type == "turn.failed" {
			hasTurnFailed = true
		}
		if e.event.Type == "message.completed" {
			t.Error("message.completed emitted after ErrAuth — must not be")
		}
	}
	if !hasTurnFailed {
		t.Error("no turn.failed event emitted for ErrAuth")
	}
}

// TestErrorClass_InfraError_NotFedToModel verifies that the raw infra error text does not appear in any persisted
// message (would be sent to the model).
func TestErrorClass_InfraError_NotFedToModel(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	const secretDetail = "VERY_INTERNAL_SECRET_DB_ERROR_DETAIL"
	infraErr := errors.New(secretDetail)
	gw := &errorChatter{err: infraErr}

	reg := capability.NewRegistry()
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	startTurnForErrors(t, h, s, p, convID)

	waitForTurnFailed(t, bus, 5*time.Second)

	// Check that the secret detail is not in any persisted message.
	scope := store.UserScope(p)
	sq := s.ForUser(scope)
	msgs, err := sq.ListMessages(context.Background(), convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, secretDetail) {
			t.Errorf("persisted message role=%q contains internal error detail: %q", m.Role, m.Content)
		}
	}
}

// ── AC-5: panic recovery emits turn.failed ────────────────────────────────────

// TestErrorClass_Panic_EmitsTurnFailed verifies that a panic in the gateway is caught, does not crash the
// goroutine, and emits turn.failed.
func TestErrorClass_Panic_EmitsTurnFailed(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	gw := makePanicChatter("something exploded in the gateway")
	reg := capability.NewRegistry()
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	startTurnForErrors(t, h, s, p, convID)

	// If panic is not recovered, the goroutine crashes and turn.failed never fires — the test will timeout.
	evs := waitForTurnFailed(t, bus, 5*time.Second)

	var hasTurnFailed bool
	for _, e := range evs {
		if e.event.Type == "turn.failed" {
			hasTurnFailed = true
		}
		if e.event.Type == "message.completed" {
			t.Error("message.completed emitted after panic — must not be")
		}
	}
	if !hasTurnFailed {
		t.Error("no turn.failed event emitted after panic — goroutine likely crashed")
	}
}

// TestErrorClass_ToolPanic_EmitsTurnFailed verifies that a panic in a tool's Execute function is caught and emits
// turn.failed (panics in tools are infra).
func TestErrorClass_ToolPanic_EmitsTurnFailed(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	// Build a tool that panics.
	type args struct{}
	panicTool := capability.NewTool(
		"panic_tool", "always panics",
		[]byte(`{"type":"object","properties":{}}`),
		capability.RiskRead,
		func(_ context.Context, _ store.Scope, _ args) (capability.Result, error) {
			panic("tool panic!")
		},
	)

	reg := capability.NewRegistry()
	if err := reg.Add(fakeSource{tools: []capability.Tool{panicTool}}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	const callID = "call_panic"
	round1 := []gateway.Chunk{
		toolCallChunk(callID, "panic_tool", `{}`),
		{Done: true},
	}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1}}
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	startTurnForErrors(t, h, s, p, convID)

	evs := waitForTurnFailed(t, bus, 5*time.Second)

	var hasTurnFailed bool
	for _, e := range evs {
		if e.event.Type == "turn.failed" {
			hasTurnFailed = true
		}
	}
	if !hasTurnFailed {
		t.Error("no turn.failed event emitted after tool panic")
	}
}

// ── AC-1 supplement: unknown tool uses tool.failed path ──────────────────────

// TestErrorClass_UnknownTool_EmitsToolFailed verifies that an unknown tool name now goes through the tool.failed +
// continue path (not raw error stub).
func TestErrorClass_UnknownTool_EmitsToolFailed(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	// Empty registry.
	reg := capability.NewRegistry()
	const callID = "call_unk2"
	round1 := []gateway.Chunk{toolCallChunk(callID, "nonexistent_tool", `{}`), {Done: true}}
	round2 := []gateway.Chunk{{TextDelta: "I see, that tool doesn't exist"}, {Done: true}}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1, round2}}
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	startTurnForErrors(t, h, s, p, convID)

	// Wait for message.completed — loop must continue.
	evs := waitForCompleted(t, bus, 1, 5*time.Second)

	// tool.failed must be emitted.
	var toolFailedEvts []harness.ToolFailedPayload
	for _, e := range evs {
		if e.event.Type == "tool.failed" {
			p, ok := e.event.Data.(harness.ToolFailedPayload)
			if ok {
				toolFailedEvts = append(toolFailedEvts, p)
			}
		}
	}
	if len(toolFailedEvts) == 0 {
		t.Fatal("no tool.failed event for unknown tool — should use tool.failed+continue path")
	}
	if toolFailedEvts[0].CallID != callID {
		t.Errorf("tool.failed callId = %q, want %q", toolFailedEvts[0].CallID, callID)
	}
	if !strings.Contains(toolFailedEvts[0].Error, "nonexistent_tool") {
		t.Errorf("tool.failed error = %q, want it to mention the tool name", toolFailedEvts[0].Error)
	}

	// Loop continued: message.completed must be emitted.
	var completedCount int
	for _, e := range evs {
		if e.event.Type == "message.completed" {
			completedCount++
		}
	}
	if completedCount != 1 {
		t.Errorf("message.completed count = %d, want 1 (loop should continue after unknown tool)", completedCount)
	}
}

// ── ErrContextLength routing ──────────────────────────────────────────────────

// TestErrorClass_ErrContextLength_RoutesToSeam verifies that ErrContextLength routes to the context-length seam
// and emits exactly ONE terminal event: message.completed with finishReason "context_length". It must NOT emit
// turn.failed — that would be a contradictory double-terminal on the same turn.
func TestErrorClass_ErrContextLength_RoutesToSeam(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	gw := &errorChatter{err: gateway.ErrContextLength}
	reg := capability.NewRegistry()
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	startTurnForErrors(t, h, s, p, convID)

	// Wait for message.completed — the one and only terminal event for this path.
	deadline := time.Now().Add(5 * time.Second)
	var evs []fakeEvent
	for time.Now().Before(deadline) {
		evs = bus.snapshot()
		for _, e := range evs {
			if e.event.Type == "message.completed" {
				goto done
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
done:

	// Assert: exactly one message.completed with finishReason "context_length".
	var completedEvts []harness.CompletedPayload
	var turnFailedEvts []harness.TurnFailedPayload
	for _, e := range evs {
		switch e.event.Type {
		case "message.completed":
			payload, ok := e.event.Data.(harness.CompletedPayload)
			if !ok {
				t.Errorf("message.completed Data type = %T, want harness.CompletedPayload", e.event.Data)
				continue
			}
			completedEvts = append(completedEvts, payload)
		case "turn.failed":
			payload, ok := e.event.Data.(harness.TurnFailedPayload)
			if !ok {
				t.Errorf("turn.failed Data type = %T, want harness.TurnFailedPayload", e.event.Data)
				continue
			}
			turnFailedEvts = append(turnFailedEvts, payload)
		}
	}

	// Exactly one message.completed must fire.
	if len(completedEvts) != 1 {
		t.Errorf("message.completed count = %d, want exactly 1", len(completedEvts))
	} else if completedEvts[0].FinishReason != "context_length" {
		t.Errorf("message.completed finishReason = %q, want %q", completedEvts[0].FinishReason, "context_length")
	}

	// Zero turn.failed events — a client treats message.completed and turn.failed as mutually exclusive terminal
	// events; emitting both is a protocol violation.
	if len(turnFailedEvts) != 0 {
		t.Errorf("turn.failed count = %d, want 0 — context_length must not emit turn.failed alongside message.completed", len(turnFailedEvts))
	}
}

// ── AC-4 supplement: tool.failed carries ONLY model-safe message ──────────────

// TestErrorClass_ToolFailed_ModelSafeMessageOnly verifies AC-4: the tool.failed event carries exactly the
// *ToolError.Message and nothing from the wrapped error.
func TestErrorClass_ToolFailed_ModelSafeMessageOnly(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	const (
		modelSafeMessage = "the provided date format is invalid"
		internalWrapped  = "time.Parse: cannot parse 'foo' as RFC3339"
	)

	// The tool internally wraps an error but returns a *ToolError with only the safe message.
	type args struct{}
	leakyTool := capability.NewTool(
		"leaky_tool", "has internal errors",
		[]byte(`{"type":"object","properties":{}}`),
		capability.RiskRead,
		func(_ context.Context, _ store.Scope, _ args) (capability.Result, error) {
			// The tool returns only the model-safe ToolError; the internal detail is deliberately not in
			// ToolError.Message.
			return nil, &capability.ToolError{
				Code:    "invalid_date",
				Message: modelSafeMessage,
			}
		},
	)

	reg := capability.NewRegistry()
	if err := reg.Add(fakeSource{tools: []capability.Tool{leakyTool}}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	const callID = "call_leaky"
	round1 := []gateway.Chunk{toolCallChunk(callID, "leaky_tool", `{}`), {Done: true}}
	round2 := []gateway.Chunk{{TextDelta: "understood"}, {Done: true}}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1, round2}}
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	startTurnForErrors(t, h, s, p, convID)

	evs := waitForCompleted(t, bus, 1, 5*time.Second)

	for _, e := range evs {
		if e.event.Type != "tool.failed" {
			continue
		}
		p, ok := e.event.Data.(harness.ToolFailedPayload)
		if !ok {
			t.Fatalf("tool.failed Data type = %T, want harness.ToolFailedPayload", e.event.Data)
		}
		if p.Error != modelSafeMessage {
			t.Errorf("tool.failed error = %q, want exactly %q", p.Error, modelSafeMessage)
		}
		if strings.Contains(p.Error, internalWrapped) {
			t.Errorf("tool.failed error leaks internal wrapped error: %q", p.Error)
		}
	}
}

// fakeSource is defined in harness_tools_test.go (same package); used directly above. The events import is used
// via events.Publisher in the type assertion below.
var _ events.Publisher = (*fakeBus)(nil)
