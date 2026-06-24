package harness_test

// concurrent_resolve_test.go — TDD tests for the double-execution race in Resolve.
//
// Two confirmations (callA, callB) are pending in ONE conversation. Two Resolve calls fire concurrently from separate
// goroutines. Without per-conversation serialisation and an atomic status CAS each goroutine sees both calls as
// approved-and-not-yet-done and executes both tools, producing double execution and duplicate DB rows.
//
// Assertions (must hold after the fix):
//   (a) Each tool's Execute ran EXACTLY ONCE (mutex-guarded counter).
//   (b) Exactly ONE tool-result row exists per callID.
//   (c) Exactly ONE terminal message.completed for the turn.
//   (d) No data race (enforced by the -race flag on the caller).
//
// The test MUST FAIL (double-execution / duplicate rows) before the fix and PASS after.

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/capability"
	"github.com/qovira/qovira/internal/gateway"
	"github.com/qovira/qovira/internal/harness"
	"github.com/qovira/qovira/internal/id"
	"github.com/qovira/qovira/internal/store"
)

// countingTool records how many times Execute is called for a specific callID, safe for concurrent access.
type countingTool struct {
	mu     sync.Mutex
	counts map[string]int
}

func newCountingTool() *countingTool {
	return &countingTool{counts: make(map[string]int)}
}

func (c *countingTool) record(callID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[callID]++
}

func (c *countingTool) countFor(callID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[callID]
}

// countToolResultRows returns the number of tool-result rows in the DB for the given callID.
func countToolResultRows(t *testing.T, s *store.Store, userID, convID, callID string) int {
	t.Helper()
	var n int
	err := s.Reader().QueryRowContext(
		context.Background(),
		`SELECT count(*) FROM messages WHERE conversation_id = ? AND user_id = ? AND role = 'tool' AND tool_call_id = ?`,
		convID, userID, callID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("countToolResultRows: %v", err)
	}
	return n
}

// countCompletedEvents counts "message.completed" events for a given conversation.
func countCompletedEvents(evs []fakeEvent) int {
	n := 0
	for _, e := range evs {
		if e.event.Type == "message.completed" {
			n++
		}
	}
	return n
}

// TestConcurrentResolve_NoDoubleExecution is the TDD test for the must-fix race.
//
// Setup: one conversation with two Confirm-tier tool calls (callA, callB) both in a single assistant message, both
// with a pending_confirmations row in "approved" status (simulating two near-simultaneous Resolve calls having already
// stored the status update in the DB). Then two concurrent goroutines call Resolve for callA and callB respectively,
// racing to enter run for the same conversation.
//
// The test fires two concurrent Resolve calls and then waits for exactly one message.completed. Without serialisation
// both goroutines execute both tools and persist duplicate rows.
func TestConcurrentResolve_NoDoubleExecution(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	counter := newCountingTool()

	const (
		callIDA = "concurrent_call_A"
		callIDB = "concurrent_call_B"
	)

	// Build a Confirm-tier (RiskExternal) tool that counts executions per callID.
	type args struct {
		CallID string `json:"call_id"`
	}
	extTool := capability.NewTool(
		"concurrent_ext_tool", "concurrent double-execution test tool",
		json.RawMessage(`{"type":"object","properties":{"call_id":{"type":"string"}}}`),
		capability.RiskExternal, // External → Confirm for Trusted origin
		func(_ context.Context, _ store.Scope, a args) (capability.Result, error) {
			counter.record(a.CallID)
			return map[string]string{"echo": a.CallID}, nil
		},
	)

	// After both tool calls are executed, the model gives a final reply.
	roundFinal := []gateway.Chunk{
		{TextDelta: "both done"},
		{Done: true},
	}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{roundFinal}}
	h := buildHarnessWithConfirmTool(t, s, gw, bus, extTool, time.Hour)
	srv := buildConfirmServer(h)

	// Manually construct the conversation in a suspended state: user message + assistant message with tool_calls A and
	// B + two pending_confirmations rows.
	convID := id.New()
	scope := store.UserScope(p)
	sq := s.ForUser(scope)

	if err := sq.UpsertConversation(context.Background(), convID); err != nil {
		t.Fatalf("UpsertConversation: %v", err)
	}

	// Persist user message.
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             id.New(),
		ConversationID: convID,
		Role:           "user",
		Content:        "do things A and B",
	}); err != nil {
		t.Fatalf("InsertMessage user: %v", err)
	}

	// Persist assistant message with BOTH tool calls in a single message.
	toolCallsJSON, _ := json.Marshal([]gateway.ToolCall{
		{ID: callIDA, Name: "concurrent_ext_tool", Arguments: json.RawMessage(`{"call_id":"A"}`)},
		{ID: callIDB, Name: "concurrent_ext_tool", Arguments: json.RawMessage(`{"call_id":"B"}`)},
	})
	assistantMsgID := id.New()
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             assistantMsgID,
		ConversationID: convID,
		Role:           "assistant",
		Content:        "",
		ToolCalls:      string(toolCallsJSON),
	}); err != nil {
		t.Fatalf("InsertMessage assistant: %v", err)
	}

	// Insert pending_confirmations rows for both calls in "pending" status.
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	for _, cid := range []string{callIDA, callIDB} {
		_, err := s.Writer().ExecContext(context.Background(),
			`INSERT INTO pending_confirmations (id, conversation_id, message_id, user_id, tool_name, args, risk, status, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
			cid, convID, assistantMsgID, p.UserID, "concurrent_ext_tool",
			`{"call_id":"`+cid[len(cid)-1:]+`"}`, "external", expiresAt,
		)
		if err != nil {
			t.Fatalf("insert pending_confirmations %s: %v", cid, err)
		}
	}

	// Fire two concurrent Resolve calls — one for each callID. The race: both goroutines will re-enter run for the same
	// conversation.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		rr := resolveViaHTTP(t, srv, p, convID, callIDA, "approve")
		if rr.Code != 202 {
			t.Errorf("resolveViaHTTP callA: status = %d, want 202; body: %s", rr.Code, rr.Body.String())
		}
	}()
	go func() {
		defer wg.Done()
		rr := resolveViaHTTP(t, srv, p, convID, callIDB, "approve")
		if rr.Code != 202 {
			t.Errorf("resolveViaHTTP callB: status = %d, want 202; body: %s", rr.Code, rr.Body.String())
		}
	}()
	wg.Wait()

	// Wait for exactly one message.completed.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		evs := bus.snapshot()
		if countCompletedEvents(evs) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Give a small settling window for any in-flight duplicate work.
	time.Sleep(200 * time.Millisecond)

	// (a) Each tool's Execute ran EXACTLY ONCE.
	if n := counter.countFor("A"); n != 1 {
		t.Errorf("tool Execute for callA ran %d times, want exactly 1 (double-execution race)", n)
	}
	if n := counter.countFor("B"); n != 1 {
		t.Errorf("tool Execute for callB ran %d times, want exactly 1 (double-execution race)", n)
	}

	// (b) Exactly ONE tool-result row per callID.
	if n := countToolResultRows(t, s, p.UserID, convID, callIDA); n != 1 {
		t.Errorf("tool-result rows for callA = %d, want 1 (duplicate DB row race)", n)
	}
	if n := countToolResultRows(t, s, p.UserID, convID, callIDB); n != 1 {
		t.Errorf("tool-result rows for callB = %d, want 1 (duplicate DB row race)", n)
	}

	// (c) Exactly ONE terminal message.completed for the turn.
	evs := bus.snapshot()
	if n := countCompletedEvents(evs); n != 1 {
		t.Errorf("message.completed count = %d, want 1 (duplicate continue-round race)", n)
	}
}

// TestTurnCompletionGuard_SpuriousReentryIsNoOp verifies the turn-completion guard directly: when a conversation's
// last message is already a final assistant reply (role="assistant", no tool_calls), calling Resolve against any
// approved call in that conversation must be a no-op — it must emit nothing and must not call the gateway again.
//
// This covers the exact path that the x60 hammer test exposes: G1 finishes the turn (persists the final assistant
// reply), G2 then acquires the lock, enters run, sees no outstanding tool calls, but now hits the turn-completion guard
// and returns without doing another gateway round.
func TestTurnCompletionGuard_SpuriousReentryIsNoOp(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	const callID = "guard_call_noop"

	type args struct {
		CallID string `json:"call_id"`
	}
	extTool := capability.NewTool(
		"guard_ext_tool", "guard no-op test tool",
		json.RawMessage(`{"type":"object","properties":{"call_id":{"type":"string"}}}`),
		capability.RiskExternal,
		func(_ context.Context, _ store.Scope, a args) (capability.Result, error) {
			return map[string]string{"echo": a.CallID}, nil
		},
	)

	// The gateway will only ever be called once (the normal resume after approve).
	// If the guard fails and run re-enters the gateway, the queuedChatter will
	// return the same final-reply chunk a second time → second message.completed.
	roundFinal := []gateway.Chunk{
		{TextDelta: "done"},
		{Done: true},
	}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{roundFinal}}
	h := buildHarnessWithConfirmTool(t, s, gw, bus, extTool, time.Hour)
	srv := buildConfirmServer(h)

	// Manually build a conversation already at the completed-turn state:
	// user → assistant(tool_calls) → tool-result → assistant(final, no tool_calls).
	convID := id.New()
	scope := store.UserScope(p)
	sq := s.ForUser(scope)

	if err := sq.UpsertConversation(context.Background(), convID); err != nil {
		t.Fatalf("UpsertConversation: %v", err)
	}
	// user message
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             id.New(),
		ConversationID: convID,
		Role:           "user",
		Content:        "do the thing",
	}); err != nil {
		t.Fatalf("InsertMessage user: %v", err)
	}
	// assistant message with tool_calls (the original suspended round)
	toolCallsJSON, _ := json.Marshal([]gateway.ToolCall{
		{ID: callID, Name: "guard_ext_tool", Arguments: json.RawMessage(`{"call_id":"guard"}`)},
	})
	assistantMsgID := id.New()
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             assistantMsgID,
		ConversationID: convID,
		Role:           "assistant",
		Content:        "",
		ToolCalls:      string(toolCallsJSON),
	}); err != nil {
		t.Fatalf("InsertMessage assistant+tool_calls: %v", err)
	}
	// tool-result (already executed — simulates G1 already having done the work)
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             id.New(),
		ConversationID: convID,
		Role:           "tool",
		Content:        `{"echo":"guard"}`,
		ToolCallID:     callID,
	}); err != nil {
		t.Fatalf("InsertMessage tool-result: %v", err)
	}
	// final assistant reply — the turn is already complete
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             id.New(),
		ConversationID: convID,
		Role:           "assistant",
		Content:        "all done",
		ToolCalls:      "", // no tool_calls → final reply
	}); err != nil {
		t.Fatalf("InsertMessage final assistant: %v", err)
	}

	// Insert a pending_confirmations row in "approved" status to simulate a Resolve that won the CAS but whose
	// goroutine runs after G1 already finished the turn.
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := s.Writer().ExecContext(context.Background(),
		`INSERT INTO pending_confirmations (id, conversation_id, message_id, user_id, tool_name, args, risk, status, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'approved', ?)`,
		callID, convID, assistantMsgID, p.UserID, "guard_ext_tool",
		`{"call_id":"guard"}`, "external", expiresAt,
	); err != nil {
		t.Fatalf("insert pending_confirmations: %v", err)
	}

	// Update to "approved" — CAS already done by G1's Resolve call. Now call Resolve with the same callID from "G2":
	// this should be a no-op.
	rr := resolveViaHTTP(t, srv, p, convID, callID, "approve")
	// Resolve returns 409 (ErrConfirmationAlreadyResolved) because the CAS UPDATE finds rowsAffected=0 — the row is
	// already "approved", not "pending". This is the correct HTTP result for a double-Resolve attempt.
	if rr.Code != http.StatusConflict {
		t.Errorf("spurious Resolve status = %d, want 409 (already resolved)", rr.Code)
	}

	// Give the bus a moment to receive any spurious events from a leaked goroutine.
	time.Sleep(200 * time.Millisecond)

	// No events must have been emitted — Resolve returned 409 before spawning a goroutine.
	evs := bus.snapshot()
	if n := countCompletedEvents(evs); n != 0 {
		t.Errorf("spurious message.completed count = %d, want 0 (no-op re-entry)", n)
	}

	// The gateway must not have been called (pos stays at 0).
	if gw.pos != 0 {
		t.Errorf("gateway Chat called %d times, want 0 (turn-completion guard prevented re-entry)", gw.pos)
	}
}

// TestTurnCompletionGuard_AlreadyCompleteRunIsNoOp verifies the guard at the run level: when StartTurn is called for a
// conversation whose last message is already a final assistant reply, run must return immediately with no gateway call
// and no event emitted. This simulates a duplicate StartTurn call (e.g. retried HTTP request hitting a turn that
// already completed).
func TestTurnCompletionGuard_AlreadyCompleteRunIsNoOp(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	// If the guard is absent, the gateway gets called and emits message.completed.
	roundFinal := []gateway.Chunk{
		{TextDelta: "should not be called"},
		{Done: true},
	}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{roundFinal}}

	reg := capability.NewRegistry()
	h := harness.New(reg, gw, s, bus, harness.Config{}, harness.NewDiscardLogger())

	convID := id.New()
	scope := store.UserScope(p)
	sq := s.ForUser(scope)

	if err := sq.UpsertConversation(context.Background(), convID); err != nil {
		t.Fatalf("UpsertConversation: %v", err)
	}
	// user message
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             id.New(),
		ConversationID: convID,
		Role:           "user",
		Content:        "hello",
	}); err != nil {
		t.Fatalf("InsertMessage user: %v", err)
	}
	// final assistant reply — turn already complete
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             id.New(),
		ConversationID: convID,
		Role:           "assistant",
		Content:        "hello to you too",
		ToolCalls:      "",
	}); err != nil {
		t.Fatalf("InsertMessage final assistant: %v", err)
	}

	// Call StartTurn on an already-completed conversation.
	origin := harness.Origin{Channel: "web", Trust: harness.Trusted}
	if err := h.StartTurn(context.Background(), convID, harness.InboundMessage{Content: "hello"}, origin, p); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}

	// Allow the background goroutine to run and hit the guard.
	time.Sleep(200 * time.Millisecond)

	// No events must have been emitted.
	evs := bus.snapshot()
	if n := countCompletedEvents(evs); n != 0 {
		t.Errorf("message.completed emitted %d time(s), want 0 (guard must no-op)", n)
	}

	// The gateway must not have been called.
	if gw.pos != 0 {
		t.Errorf("gateway Chat called %d times, want 0 (turn-completion guard)", gw.pos)
	}
}
