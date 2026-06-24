package harness_test

// Correctness regression tests for H1 work unit. Tests are TDD: written to fail against the original code and pass
// after the fix.

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/qovira/qovira/internal/capability"
	"github.com/qovira/qovira/internal/gateway"
	"github.com/qovira/qovira/internal/harness"
	"github.com/qovira/qovira/internal/id"
	"github.com/qovira/qovira/internal/store"
)

// ── Correctness #1: empty tool-call ID executes exactly once ─────────────────
//
// The gateway never synthesizes a tool-call ID; an empty ID arrives in the streamed ToolCall. Before the fix the
// NULL-id result never entered the idempotency sets so the tool re-ran every step until stepCap. After the fix the
// harness synthesises a unique ID for any empty-ID call before persistence so the idempotency check fires and the
// tool runs once.

func TestToolCall_EmptyID_ExecutesExactlyOnce(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	var execCount atomic.Int64
	reg := capability.NewRegistry()
	tool := capability.NewTool(
		"counter_tool", "counts executions",
		json.RawMessage(`{"type":"object","properties":{}}`),
		capability.RiskRead,
		func(_ context.Context, _ store.Scope, _ struct{}) (capability.Result, error) {
			execCount.Add(1)
			return map[string]string{"ok": "yes"}, nil
		},
	)
	if err := reg.Add(fakeSource{tools: []capability.Tool{tool}}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	// Round 1: tool call with an EMPTY ID (simulates what the real gateway emits).
	// Round 2: plain text reply to terminate the turn.
	round1 := []gateway.Chunk{
		// ID: "" — the bug trigger.
		{ToolCall: &gateway.ToolCall{ID: "", Name: "counter_tool", Arguments: json.RawMessage(`{}`)}},
		{Done: true},
	}
	round2 := []gateway.Chunk{{TextDelta: "done"}, {Done: true}}

	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1, round2}}

	const stepCap = 4
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{StepCap: stepCap})

	convID := id.New()
	// Wait for the terminal message.completed; at least 1 tool.started must fire.
	startTurnAndWait(t, h, s, p, bus, convID, 1)

	got := int(execCount.Load())
	if got != 1 {
		t.Errorf("counter_tool executed %d time(s), want exactly 1 (empty-ID idempotency regression)", got)
	}

	// Also confirm the turn ended with message.completed, not step_cap.
	evs := bus.snapshot()
	var completedCount int
	for _, e := range evs {
		if e.event.Type == "message.completed" {
			completedCount++
		}
	}
	if completedCount != 1 {
		t.Errorf("message.completed count = %d, want 1", completedCount)
	}
}

// ── Correctness #2: persistAssistantMessage records finish_reason correctly ───
//
// Before the fix persistAssistantMessage hardcoded finish_reason="stop". A tool-call round must record "tool_calls"
// and a plain reply must record "stop".

func TestPersistAssistantMessage_FinishReason(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	reg := capability.NewRegistry()
	tool := capability.NewTool(
		"noop", "noop",
		json.RawMessage(`{"type":"object","properties":{}}`),
		capability.RiskRead,
		func(_ context.Context, _ store.Scope, _ struct{}) (capability.Result, error) {
			return map[string]string{"ok": "1"}, nil
		},
	)
	if err := reg.Add(fakeSource{tools: []capability.Tool{tool}}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	// Round 1: one tool call → assistant message must have finish_reason="tool_calls".
	// Round 2: plain text reply → assistant message must have finish_reason="stop".
	const callID = "call_fr_test"
	round1 := []gateway.Chunk{
		toolCallChunk(callID, "noop", `{}`),
		{Done: true},
	}
	round2 := []gateway.Chunk{{TextDelta: "final"}, {Done: true}}

	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1, round2}}
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	startTurnAndWait(t, h, s, p, bus, convID, 1)

	scope := store.UserScope(p)
	sq := s.ForUser(scope)
	msgs, err := sq.ListMessages(context.Background(), convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}

	// Walk messages and collect finish_reason for each assistant message.
	// First assistant message (round 1) has tool_calls → "tool_calls".
	// Second assistant message (round 2) is the final reply → "stop".
	type assistantMsg struct {
		hasToolCalls bool
		finishReason string
	}
	var assistants []assistantMsg
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		hasTCs := m.ToolCalls.Valid && m.ToolCalls.String != ""
		fr := ""
		if m.FinishReason.Valid {
			fr = m.FinishReason.String
		}
		assistants = append(assistants, assistantMsg{hasToolCalls: hasTCs, finishReason: fr})
	}

	if len(assistants) < 2 {
		t.Fatalf("expected 2 assistant messages, got %d", len(assistants))
	}

	// First assistant message: had tool calls → finish_reason must be "tool_calls".
	if !assistants[0].hasToolCalls {
		t.Error("first assistant message has no tool_calls (test setup error)")
	}
	if assistants[0].finishReason != "tool_calls" {
		t.Errorf("first assistant finish_reason = %q, want %q", assistants[0].finishReason, "tool_calls")
	}

	// Second assistant message: plain reply → finish_reason must be "stop".
	if assistants[1].hasToolCalls {
		t.Error("second assistant message unexpectedly has tool_calls (test setup error)")
	}
	if assistants[1].finishReason != "stop" {
		t.Errorf("second assistant finish_reason = %q, want %q", assistants[1].finishReason, "stop")
	}
}

// ── Cleanup #7: logged panic value is bounded/typed ───────────────────────────
//
// Raw panic values can embed sensitive data. After the fix the logged "panic" field is a sanitised, bounded,
// type-tagged string (sanitizePanic): capped at 256 bytes on a UTF-8 rune boundary and prefixed with the value's
// dynamic type, not the raw recovered interface{} value.
//
// The test injects a panic whose string form exceeds the cap AND has a multi-byte rune straddling the byte-256
// boundary, captures the harness log via a recording slog.Handler, and asserts the field is type-tagged,
// truncated, well-formed UTF-8, and within the cap.

// capturingHandler is a minimal slog.Handler that records each record's attributes (merged with any WithAttrs
// context) under a mutex, so a test can read a logged field even though the harness logs from a background
// goroutine.
type capturingHandler struct {
	mu      *sync.Mutex
	records *[]map[string]any
	attrs   []slog.Attr
}

func newCapturingHandler() *capturingHandler {
	return &capturingHandler{mu: &sync.Mutex{}, records: &[]map[string]any{}}
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	m := map[string]any{"msg": r.Message}
	for _, a := range h.attrs {
		m[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	*h.records = append(*h.records, m)
	h.mu.Unlock()
	return nil
}

func (h *capturingHandler) WithAttrs(as []slog.Attr) slog.Handler {
	merged := append(append([]slog.Attr{}, h.attrs...), as...)
	return &capturingHandler{mu: h.mu, records: h.records, attrs: merged}
}

func (h *capturingHandler) WithGroup(string) slog.Handler { return h }

// field returns the value logged under key in the first record carrying it.
func (h *capturingHandler) field(key string) (any, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range *h.records {
		if v, ok := m[key]; ok {
			return v, true
		}
	}
	return nil, false
}

func TestPanic_LoggedValueIsBounded(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	// 255 ASCII bytes, then a run of 'é' (2 bytes each: 0xC3 0xA9). The first 'é' starts at byte 255, so byte 256 —
	// the raw cap offset — lands on its continuation byte. A naive byte slice would split it; sanitizePanic must back
	// up to byte 255 and keep the logged output valid UTF-8.
	panicMsg := strings.Repeat("a", 255) + strings.Repeat("é", 50)
	gw := makePanicChatter(panicMsg)

	reg := capability.NewRegistry()
	recorder := newCapturingHandler()
	h := harness.New(reg, gw, s, bus, harness.Config{}, slog.New(recorder))

	convID := id.New()
	scope := store.UserScope(p)
	sq := s.ForUser(scope)
	if err := sq.UpsertConversation(context.Background(), convID); err != nil {
		t.Fatalf("UpsertConversation: %v", err)
	}
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             id.New(),
		ConversationID: convID,
		Role:           "user",
		Content:        "trigger panic",
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	origin := harness.Origin{Channel: "web", Trust: harness.Trusted}
	if err := h.StartTurn(context.Background(), convID, harness.InboundMessage{Content: "trigger panic"}, origin, p); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}

	// Wait for turn.failed (the panic recovery emits it after logging "panic").
	deadline := time.Now().Add(5 * time.Second)
	failed := false
	for time.Now().Before(deadline) && !failed {
		for _, e := range bus.snapshot() {
			if e.event.Type == "turn.failed" {
				failed = true
				break
			}
		}
		if !failed {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !failed {
		t.Fatal("turn.failed event not received after panic — panic may not have been recovered")
	}

	v, ok := recorder.field("panic")
	if !ok {
		t.Fatal(`no "panic" field was logged on the recovery path`)
	}
	got, ok := v.(string)
	if !ok {
		t.Fatalf(`logged "panic" field is %T, want a sanitised string`, v)
	}
	if !utf8.ValidString(got) {
		t.Errorf("logged panic field is not valid UTF-8 (a rune was split): %q", got)
	}
	// Type-tagged: makePanicChatter panics with a string value → "%T" == "string".
	if !strings.HasPrefix(got, "string: ") {
		t.Errorf("logged panic field is not type-tagged: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("logged panic field was not truncated: %q", got)
	}
	// The message body (sans "string: " prefix and "…" suffix) must stay within the 256-byte cap, and the full raw
	// payload must not have leaked.
	body := strings.TrimSuffix(strings.TrimPrefix(got, "string: "), "…")
	if len(body) > 256 {
		t.Errorf("logged panic message body = %d bytes, want <= 256", len(body))
	}
	if strings.Contains(got, panicMsg) {
		t.Error("logged panic field contains the full raw panic payload")
	}
}
