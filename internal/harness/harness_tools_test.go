package harness_test

// Tool-call multi-round tests (AC-1 through AC-6 from the harness tool-call issue).
// Each test is independently verifiable; fakes are used for all collaborators.

import (
	"context"
	"encoding/json"
	"iter"
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

// ── multi-round fake chatter ──────────────────────────────────────────────────

// queuedChatter returns a different set of chunks per call to Chat, in queue
// order. Each element is the full chunk sequence for one round. When the queue
// is exhausted it wraps (modulo) — useful for "always tool call" scenarios.
type queuedChatter struct {
	mu     sync.Mutex
	rounds [][]gateway.Chunk
	pos    int
}

func (q *queuedChatter) Chat(_ context.Context, _ gateway.ChatRequest) (iter.Seq2[gateway.Chunk, error], error) {
	q.mu.Lock()
	chunks := q.rounds[q.pos%len(q.rounds)]
	q.pos++
	q.mu.Unlock()

	seq := func(yield func(gateway.Chunk, error) bool) {
		for _, c := range chunks {
			if !yield(c, nil) {
				return
			}
		}
	}
	return seq, nil
}

// ── capability test helpers ───────────────────────────────────────────────────

// fakeSource is a minimal capability.Module test double for the harness tests.
type fakeSource struct {
	tools []capability.Tool
}

func (f fakeSource) Tools() []capability.Tool { return f.tools }

// ── test tool helpers ─────────────────────────────────────────────────────────

// callCapture records call IDs in order, safe for concurrent use.
type callCapture struct {
	mu      sync.Mutex
	callIDs []string
}

func (c *callCapture) record(callID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.callIDs = append(c.callIDs, callID)
}

func (c *callCapture) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.callIDs))
	copy(out, c.callIDs)
	return out
}

// makeEchoTool builds a RiskRead tool that records the call_id argument in
// tracker and returns {"echo": <call_id>}.
func makeEchoTool(name string, tracker *callCapture) capability.Tool {
	type args struct {
		CallID string `json:"call_id"`
	}
	return capability.NewTool(
		name, "echo call id",
		json.RawMessage(`{"type":"object","properties":{"call_id":{"type":"string"}}}`),
		capability.RiskRead,
		func(_ context.Context, _ store.Scope, a args) (capability.Result, error) {
			tracker.record(a.CallID)
			return map[string]string{"echo": a.CallID}, nil
		},
	)
}

// toolCallChunk builds a Chunk carrying a ToolCall.
func toolCallChunk(callID, name string, argsJSON string) gateway.Chunk {
	return gateway.Chunk{ToolCall: &gateway.ToolCall{
		ID:        callID,
		Name:      name,
		Arguments: json.RawMessage(argsJSON),
	}}
}

// buildHarnessWithTools builds a Harness wired with the given registry and cfg.
func buildHarnessWithTools(
	t *testing.T,
	s *store.Store,
	gw harness.Chatter,
	bus events.Publisher,
	reg *capability.Registry,
	cfg harness.Config,
) *harness.Harness {
	t.Helper()
	return harness.New(reg, gw, s, bus, cfg, harness.NewDiscardLogger())
}

// startTurnAndWait fires a turn by inserting a user message and calling StartTurn,
// then blocks until the bus carries a "message.completed" event and at least
// minToolStarted "tool.started" events. Waiting for the terminal event (rather
// than a raw event count) prevents the helper from returning mid-turn before
// message.completed fires, which caused flaky completedCount assertions under
// -race.
func startTurnAndWait(
	t *testing.T,
	h *harness.Harness,
	s *store.Store,
	p store.Principal,
	bus *fakeBus,
	convID string,
	minToolStarted int,
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
	waitForCompleted(t, bus, minToolStarted, 5*time.Second)
}

// ── AC-1: tool calls persist assistant message + tool results, then loop ──────

// TestToolCall_AC1_PersistsAndLoops verifies:
//   - Round 1 yields a tool call then Done.
//   - The assistant message is persisted with non-empty tool_calls.
//   - The tool-result message is persisted (role=tool, tool_call_id set).
//   - Round 2 yields a text reply — turn ends with message.completed.
func TestToolCall_AC1_PersistsAndLoops(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	tracker := &callCapture{}
	reg := capability.NewRegistry()
	echo := makeEchoTool("echo_tool", tracker)
	if err := reg.Add(fakeSource{tools: []capability.Tool{echo}}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	const callID = "call_abc123"
	round1 := []gateway.Chunk{
		toolCallChunk(callID, "echo_tool", `{"call_id":"round1"}`),
		{Done: true},
	}
	round2 := []gateway.Chunk{
		{TextDelta: "final answer"},
		{Done: true},
	}

	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1, round2}}
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	// Wait until the turn terminates (message.completed) with at least 1 tool.started.
	startTurnAndWait(t, h, s, p, bus, convID, 1)

	// Verify assistant message with tool_calls persisted.
	scope := store.UserScope(p)
	sq := s.ForUser(scope)
	msgs, err := sq.ListMessages(context.Background(), convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}

	var assistantMsgs, toolResultMsgs int
	var hasToolCalls, hasToolCallID bool
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			assistantMsgs++
			if m.ToolCalls.Valid && m.ToolCalls.String != "" {
				hasToolCalls = true
			}
		case "tool":
			toolResultMsgs++
			if m.ToolCallID.Valid && m.ToolCallID.String == callID {
				hasToolCallID = true
			}
		}
	}

	if assistantMsgs == 0 {
		t.Error("no assistant messages persisted")
	}
	if !hasToolCalls {
		t.Error("assistant message missing tool_calls column")
	}
	if toolResultMsgs == 0 {
		t.Error("no tool-result messages persisted")
	}
	if !hasToolCallID {
		t.Errorf("tool-result message missing tool_call_id = %q", callID)
	}

	// Verify exactly one message.completed.
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

	// Tool was actually executed.
	called := tracker.snapshot()
	if len(called) == 0 {
		t.Error("echo_tool was not called")
	}
}

// ── AC-2: multi-round terminates on final no-tool-call reply ─────────────────

// TestToolCall_AC2_MultiRoundTerminates verifies a 2-tool-call-round turn feeds
// results back and terminates on a final no-tool-call reply with exactly one
// message.completed.
func TestToolCall_AC2_MultiRoundTerminates(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	tracker := &callCapture{}
	reg := capability.NewRegistry()
	echo := makeEchoTool("echo_tool", tracker)
	if err := reg.Add(fakeSource{tools: []capability.Tool{echo}}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	// Round 1: one tool call.
	// Round 2: another tool call.
	// Round 3: final text reply — no tool calls.
	round1 := []gateway.Chunk{toolCallChunk("call1", "echo_tool", `{"call_id":"r1"}`), {Done: true}}
	round2 := []gateway.Chunk{toolCallChunk("call2", "echo_tool", `{"call_id":"r2"}`), {Done: true}}
	round3 := []gateway.Chunk{{TextDelta: "done"}, {Done: true}}

	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1, round2, round3}}
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	// Wait for terminal state with at least 2 tool.started (one per round).
	startTurnAndWait(t, h, s, p, bus, convID, 2)

	evs := bus.snapshot()
	var completedCount int
	for _, e := range evs {
		if e.event.Type == "message.completed" {
			completedCount++
		}
	}
	if completedCount != 1 {
		t.Errorf("message.completed count = %d, want exactly 1", completedCount)
	}

	// Two tool calls executed.
	called := tracker.snapshot()
	if len(called) != 2 {
		t.Errorf("tool call count = %d, want 2", len(called))
	}
}

// ── AC-3: tool.started then tool.completed per call with correct payloads ─────

// TestToolCall_AC3_ToolEvents verifies that tool.started is emitted before
// tool.completed for each call, and the payloads carry callId, name, and risk.
func TestToolCall_AC3_ToolEvents(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	tracker := &callCapture{}
	reg := capability.NewRegistry()
	echo := makeEchoTool("echo_tool", tracker)
	if err := reg.Add(fakeSource{tools: []capability.Tool{echo}}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	const callID = "call_xyz"
	round1 := []gateway.Chunk{toolCallChunk(callID, "echo_tool", `{"call_id":"test"}`), {Done: true}}
	round2 := []gateway.Chunk{{TextDelta: "ok"}, {Done: true}}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1, round2}}
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	// Wait for terminal state with at least 1 tool.started.
	startTurnAndWait(t, h, s, p, bus, convID, 1)

	evs := bus.snapshot()

	startedIdx, completedIdx := -1, -1
	for i, e := range evs {
		switch e.event.Type {
		case "tool.started":
			startedIdx = i
		case "tool.completed":
			completedIdx = i
		}
	}

	if startedIdx < 0 {
		t.Fatal("no tool.started event emitted")
	}
	if completedIdx < 0 {
		t.Fatal("no tool.completed event emitted")
	}
	if startedIdx >= completedIdx {
		t.Errorf("tool.started (idx=%d) must come before tool.completed (idx=%d)", startedIdx, completedIdx)
	}

	// Check tool.started payload.
	started, ok := evs[startedIdx].event.Data.(harness.ToolStartedPayload)
	if !ok {
		t.Fatalf("tool.started Data type = %T, want harness.ToolStartedPayload", evs[startedIdx].event.Data)
	}
	if started.CallID != callID {
		t.Errorf("tool.started callId = %q, want %q", started.CallID, callID)
	}
	if started.Name != "echo_tool" {
		t.Errorf("tool.started name = %q, want %q", started.Name, "echo_tool")
	}
	if started.Risk == "" {
		t.Error("tool.started risk is empty")
	}

	// Check tool.completed payload.
	completed, ok := evs[completedIdx].event.Data.(harness.ToolCompletedPayload)
	if !ok {
		t.Fatalf("tool.completed Data type = %T, want harness.ToolCompletedPayload", evs[completedIdx].event.Data)
	}
	if completed.CallID != callID {
		t.Errorf("tool.completed callId = %q, want %q", completed.CallID, callID)
	}
	if completed.Result == nil {
		t.Error("tool.completed result is nil")
	}
}

// ── AC-4: step cap ends the turn gracefully ───────────────────────────────────

// TestToolCall_AC4_StepCap verifies that a gateway that always returns a tool
// call terminates after stepCap rounds with a graceful assistant message and
// exactly one message.completed, never exceeding the cap.
func TestToolCall_AC4_StepCap(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	tracker := &callCapture{}
	reg := capability.NewRegistry()
	echo := makeEchoTool("echo_tool", tracker)
	if err := reg.Add(fakeSource{tools: []capability.Tool{echo}}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	// Always returns a tool call — never terminates naturally.
	alwaysToolCall := []gateway.Chunk{
		toolCallChunk("call_inf", "echo_tool", `{"call_id":"inf"}`),
		{Done: true},
	}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{alwaysToolCall}}

	const stepCap = 3
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{StepCap: stepCap})

	convID := id.New()
	// Wait for terminal state; all stepCap tool.started events must have fired too.
	startTurnAndWait(t, h, s, p, bus, convID, stepCap)

	// Count gateway rounds: each call should produce exactly one started event per round.
	evs := bus.snapshot()
	var startedCount, completedTurnCount int
	for _, e := range evs {
		switch e.event.Type {
		case "tool.started":
			startedCount++
		case "message.completed":
			completedTurnCount++
		}
	}

	// The loop ran at most stepCap times.
	if startedCount > stepCap {
		t.Errorf("tool.started count = %d, want <= stepCap (%d)", startedCount, stepCap)
	}
	if completedTurnCount != 1 {
		t.Errorf("message.completed count = %d, want 1", completedTurnCount)
	}

	// A graceful assistant message must be persisted.
	scope := store.UserScope(p)
	sq := s.ForUser(scope)
	msgs, err := sq.ListMessages(context.Background(), convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var assistantMsgs int
	for _, m := range msgs {
		if m.Role == "assistant" {
			assistantMsgs++
		}
	}
	if assistantMsgs == 0 {
		t.Error("no graceful assistant message persisted on step-cap exit")
	}

	// Total gateway rounds must not exceed stepCap.
	if gw.pos > stepCap {
		t.Errorf("gateway Chat called %d times, want <= %d", gw.pos, stepCap)
	}
}

// ── AC-5: tool calls within a round execute in order ─────────────────────────

// TestToolCall_AC5_InOrderExecution verifies that two tool calls in a single
// round execute in the order the model emitted them, and each produces exactly
// one persisted tool-result message.
func TestToolCall_AC5_InOrderExecution(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	tracker := &callCapture{}
	reg := capability.NewRegistry()
	echo := makeEchoTool("echo_tool", tracker)
	if err := reg.Add(fakeSource{tools: []capability.Tool{echo}}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	// Round 1: two tool calls in order — call_A then call_B.
	round1 := []gateway.Chunk{
		toolCallChunk("call_A", "echo_tool", `{"call_id":"A"}`),
		toolCallChunk("call_B", "echo_tool", `{"call_id":"B"}`),
		{Done: true},
	}
	round2 := []gateway.Chunk{{TextDelta: "done"}, {Done: true}}

	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1, round2}}
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	// Wait for terminal state with both tool.started events present.
	startTurnAndWait(t, h, s, p, bus, convID, 2)

	// Tool calls executed in order.
	called := tracker.snapshot()
	if len(called) != 2 {
		t.Fatalf("tool call count = %d, want 2; got %v", len(called), called)
	}
	if called[0] != "A" {
		t.Errorf("first tool call arg = %q, want %q", called[0], "A")
	}
	if called[1] != "B" {
		t.Errorf("second tool call arg = %q, want %q", called[1], "B")
	}

	// Exactly 2 tool-result messages persisted.
	scope := store.UserScope(p)
	sq := s.ForUser(scope)
	msgs, err := sq.ListMessages(context.Background(), convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var toolResultCount int
	for _, m := range msgs {
		if m.Role == "tool" {
			toolResultCount++
		}
	}
	if toolResultCount != 2 {
		t.Errorf("tool-result message count = %d, want 2", toolResultCount)
	}
}

// ── AC-6: race-clean multi-round ──────────────────────────────────────────────

// TestToolCall_AC6_RaceClean exercises the multi-round loop under the race
// detector with all fakes (bus, gateway, registry). Running with -race is
// sufficient to detect data races in the turn goroutine.
func TestToolCall_AC6_RaceClean(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	tracker := &callCapture{}
	reg := capability.NewRegistry()
	echo := makeEchoTool("echo_tool", tracker)
	if err := reg.Add(fakeSource{tools: []capability.Tool{echo}}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	round1 := []gateway.Chunk{toolCallChunk("call1", "echo_tool", `{"call_id":"r1"}`), {Done: true}}
	round2 := []gateway.Chunk{{TextDelta: "done"}, {Done: true}}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1, round2}}
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	// Wait for terminal state with at least 1 tool.started.
	startTurnAndWait(t, h, s, p, bus, convID, 1)

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

// ── unknown tool graceful handling ───────────────────────────────────────────

// TestToolCall_UnknownTool_GracefulContinue verifies that when the model names
// a tool not in the catalog, a tool-result message is persisted noting the
// unknown tool, and the turn continues to the next round.
func TestToolCall_UnknownTool_GracefulContinue(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	// Empty registry — no tools registered.
	reg := capability.NewRegistry()
	round1 := []gateway.Chunk{toolCallChunk("call_unk", "nonexistent_tool", `{}`), {Done: true}}
	round2 := []gateway.Chunk{{TextDelta: "sorry"}, {Done: true}}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1, round2}}
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	// Wait for terminal state with at least 1 tool.started (the unknown tool still emits it).
	startTurnAndWait(t, h, s, p, bus, convID, 1)

	// A tool-result message must be persisted for the unknown tool.
	scope := store.UserScope(p)
	sq := s.ForUser(scope)
	msgs, err := sq.ListMessages(context.Background(), convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var toolResultCount int
	for _, m := range msgs {
		if m.Role == "tool" {
			toolResultCount++
		}
	}
	if toolResultCount == 0 {
		t.Error("no tool-result message persisted for unknown tool")
	}

	// Turn must still complete.
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

// ── default step cap ──────────────────────────────────────────────────────────

// TestToolCall_DefaultStepCap verifies that Config{} (zero value) applies the
// default step cap of 8 rather than spinning indefinitely.
func TestToolCall_DefaultStepCap(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	tracker := &callCapture{}
	reg := capability.NewRegistry()
	echo := makeEchoTool("echo_tool", tracker)
	if err := reg.Add(fakeSource{tools: []capability.Tool{echo}}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	// Always a tool call.
	alwaysToolCall := []gateway.Chunk{toolCallChunk("c", "echo_tool", `{"call_id":"x"}`), {Done: true}}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{alwaysToolCall}}

	// Zero-value Config — default step cap applies.
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	const defaultCap = 8
	convID := id.New()
	// Wait for terminal state; all defaultCap tool.started events must have fired before message.completed.
	startTurnAndWait(t, h, s, p, bus, convID, defaultCap)

	evs := bus.snapshot()
	var startedCount, completedCount int
	for _, e := range evs {
		switch e.event.Type {
		case "tool.started":
			startedCount++
		case "message.completed":
			completedCount++
		}
	}

	if startedCount > defaultCap {
		t.Errorf("default step cap: tool.started count = %d, want <= %d", startedCount, defaultCap)
	}
	if completedCount != 1 {
		t.Errorf("message.completed count = %d, want 1", completedCount)
	}
}
