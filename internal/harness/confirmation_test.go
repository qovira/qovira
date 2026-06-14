package harness_test

// confirmation_test.go — tests for the suspend/resume confirmation flow.
// Covers all six acceptance criteria from the issue.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/capability"
	"github.com/qovira/qovira/internal/events"
	"github.com/qovira/qovira/internal/gateway"
	"github.com/qovira/qovira/internal/harness"
	"github.com/qovira/qovira/internal/httpx"
	"github.com/qovira/qovira/internal/id"
	"github.com/qovira/qovira/internal/store"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// makeConfirmTool builds a Confirm-tier (RiskExternal) tool that records calls.
func makeConfirmTool(name string, tracker *callCapture) capability.Tool {
	type args struct {
		CallID string `json:"call_id"`
	}
	return capability.NewTool(
		name, "confirmation-required test tool",
		json.RawMessage(`{"type":"object","properties":{"call_id":{"type":"string"}}}`),
		capability.RiskExternal, // External → Confirm for Trusted origin
		func(_ context.Context, _ store.Scope, a args) (capability.Result, error) {
			tracker.record(a.CallID)
			return map[string]string{"echo": a.CallID}, nil
		},
	)
}

// buildHarnessWithConfirmTool wires a harness with a single Confirm-tier tool and a TTL config.
func buildHarnessWithConfirmTool(
	t *testing.T,
	s *store.Store,
	gw harness.Chatter,
	bus events.Publisher,
	tool capability.Tool,
	ttl time.Duration,
) *harness.Harness {
	t.Helper()
	reg := capability.NewRegistry()
	if err := reg.Add(fakeSource{tools: []capability.Tool{tool}}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}
	cfg := harness.Config{ConfirmationTTL: ttl}
	return harness.New(reg, gw, s, bus, cfg, harness.NewDiscardLogger())
}

// buildConfirmServer builds an http.Server with harness routes and injects the principal via context.
func buildConfirmServer(h *harness.Harness) *http.Server {
	router := httpx.NewRouter()
	h.Routes(router)
	return httpx.NewServer("127.0.0.1:0", "test", router, events.NewBus())
}

// waitForConfirmationRequired blocks until at least one "confirmation.required" event appears.
func waitForConfirmationRequired(t *testing.T, bus *fakeBus, timeout time.Duration) []fakeEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		evs := bus.snapshot()
		for _, e := range evs {
			if e.event.Type == "confirmation.required" {
				return evs
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	return bus.snapshot()
}

// countEventType counts events of the given type in the snapshot.
func countEventType(evs []fakeEvent, typ string) int {
	n := 0
	for _, e := range evs {
		if e.event.Type == typ {
			n++
		}
	}
	return n
}

// hasPendingConfirmation checks that a pending_confirmations row exists with the given callID
// for this user. It queries the DB directly.
func hasPendingConfirmation(t *testing.T, s *store.Store, userID, callID string) bool {
	t.Helper()
	var n int
	err := s.Reader().QueryRowContext(
		context.Background(),
		`SELECT count(*) FROM pending_confirmations WHERE id = ? AND user_id = ?`,
		callID, userID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("hasPendingConfirmation query: %v", err)
	}
	return n > 0
}

// getPendingConfirmationStatus returns the status of a pending_confirmations row.
func getPendingConfirmationStatus(t *testing.T, s *store.Store, userID, callID string) string {
	t.Helper()
	var status string
	err := s.Reader().QueryRowContext(
		context.Background(),
		`SELECT status FROM pending_confirmations WHERE id = ? AND user_id = ?`,
		callID, userID,
	).Scan(&status)
	if err != nil {
		t.Fatalf("getPendingConfirmationStatus: %v", err)
	}
	return status
}

// startTurnExpectSuspend fires a turn with a Confirm-tier tool call and waits for
// the confirmation.required event (max 2s). It returns the snapshot at that point.
// The turn goroutine ends after emitting confirmation.required — no parked goroutine.
func startTurnExpectSuspend(
	t *testing.T,
	h *harness.Harness,
	s *store.Store,
	p store.Principal,
	bus *fakeBus,
	convID string,
) []fakeEvent {
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
	if err := h.StartTurn(context.Background(), convID, harness.InboundMessage{Content: "test"}, origin, p); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	return waitForConfirmationRequired(t, bus, 2*time.Second)
}

// resolveViaHTTP posts a decision to the resume endpoint and returns the response.
func resolveViaHTTP(
	t *testing.T,
	srv *http.Server,
	p store.Principal,
	convID, callID, decision string,
) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"decision": decision})
	req := makeAuthedRequest(
		http.MethodPost,
		"/api/v1/conversations/"+convID+"/confirmations/"+callID,
		body,
		p,
	)
	rr := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rr, req)
	return rr
}

// ── AC-1: Confirm decision persists a row, emits confirmation.required, run returns ──

func TestConfirm_AC1_PersistsRowEmitsEventNoTerminal(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := &callCapture{}

	const callID = "call_confirm_ac1"
	extTool := makeConfirmTool("ext_tool", tracker)

	round1 := []gateway.Chunk{
		toolCallChunk(callID, "ext_tool", `{"call_id":"ac1"}`),
		{Done: true},
	}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1}}
	h := buildHarnessWithConfirmTool(t, s, gw, bus, extTool, time.Hour)

	convID := id.New()
	evs := startTurnExpectSuspend(t, h, s, p, bus, convID)

	// AC-1a: a pending_confirmations row must exist.
	if !hasPendingConfirmation(t, s, p.UserID, callID) {
		t.Error("AC-1: no pending_confirmations row persisted for Confirm-tier call")
	}

	// AC-1b: confirmation.required event emitted exactly once.
	n := countEventType(evs, "confirmation.required")
	if n != 1 {
		t.Errorf("AC-1: confirmation.required count = %d, want 1", n)
	}

	// AC-1c: no terminal event (no message.completed or turn.failed).
	if hasTerminalEvent(evs) {
		t.Error("AC-1: terminal event emitted on suspend — should NOT emit any terminal event")
	}

	// AC-1d: the tool was NOT executed.
	if len(tracker.snapshot()) != 0 {
		t.Error("AC-1: tool executed on suspend path — should not execute")
	}

	// AC-1e: verify confirmation.required payload fields.
	for _, e := range evs {
		if e.event.Type != "confirmation.required" {
			continue
		}
		pl, ok := e.event.Data.(harness.ConfirmationRequiredPayload)
		if !ok {
			t.Fatalf("AC-1: confirmation.required Data type = %T, want harness.ConfirmationRequiredPayload", e.event.Data)
		}
		if pl.CallID != callID {
			t.Errorf("AC-1: payload callId = %q, want %q", pl.CallID, callID)
		}
		if pl.Name != "ext_tool" {
			t.Errorf("AC-1: payload name = %q, want ext_tool", pl.Name)
		}
		if pl.Risk != "external" {
			t.Errorf("AC-1: payload risk = %q, want external", pl.Risk)
		}
		if pl.ExpiresAt == "" {
			t.Error("AC-1: payload expiresAt is empty")
		}
	}
}

// ── AC-2: Resolve(approve) re-enters run, executes tool, continues loop ──────

func TestConfirm_AC2_ApproveResumesAndExecutes(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := &callCapture{}

	const callID = "call_confirm_ac2"
	extTool := makeConfirmTool("ext_tool", tracker)

	round1 := []gateway.Chunk{
		toolCallChunk(callID, "ext_tool", `{"call_id":"ac2"}`),
		{Done: true},
	}
	// Round 2: model gives final text reply after the tool result is fed back.
	round2 := []gateway.Chunk{
		{TextDelta: "done after approval"},
		{Done: true},
	}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1, round2}}
	h := buildHarnessWithConfirmTool(t, s, gw, bus, extTool, time.Hour)
	srv := buildConfirmServer(h)

	convID := id.New()
	_ = startTurnExpectSuspend(t, h, s, p, bus, convID)

	// Approve via the HTTP endpoint.
	rr := resolveViaHTTP(t, srv, p, convID, callID, "approve")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("AC-2: POST confirmations status = %d, want 202; body: %s", rr.Code, rr.Body.String())
	}

	// Wait for the resumed turn to complete.
	waitForCompleted(t, bus, 1, 5*time.Second)

	// AC-2a: tool was executed after approval.
	if len(tracker.snapshot()) == 0 {
		t.Error("AC-2: tool not executed after approval — should execute on re-entry")
	}

	// AC-2b: tool-result persisted.
	if !hasToolResult(t, s, p, convID, callID) {
		t.Error("AC-2: tool-result not persisted after approval")
	}

	// AC-2c: turn completed (message.completed emitted).
	evs := bus.snapshot()
	if countEventType(evs, "message.completed") != 1 {
		t.Errorf("AC-2: message.completed count = %d, want 1", countEventType(evs, "message.completed"))
	}

	// AC-2d: confirmation row status updated to approved.
	st := getPendingConfirmationStatus(t, s, p.UserID, callID)
	if st != "approved" {
		t.Errorf("AC-2: row status = %q, want approved", st)
	}
}

// ── AC-3: Resolve(deny) re-enters run, synthetic declined result, model acks ─

func TestConfirm_AC3_DenyFeedsSyntheticResult(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := &callCapture{}

	const callID = "call_confirm_ac3"
	extTool := makeConfirmTool("ext_tool", tracker)

	round1 := []gateway.Chunk{
		toolCallChunk(callID, "ext_tool", `{"call_id":"ac3"}`),
		{Done: true},
	}
	// Round 2: model receives the "declined by user" result and gives an ack.
	round2 := []gateway.Chunk{
		{TextDelta: "understood, I won't do that"},
		{Done: true},
	}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1, round2}}
	h := buildHarnessWithConfirmTool(t, s, gw, bus, extTool, time.Hour)
	srv := buildConfirmServer(h)

	convID := id.New()
	_ = startTurnExpectSuspend(t, h, s, p, bus, convID)

	// Deny via the HTTP endpoint.
	rr := resolveViaHTTP(t, srv, p, convID, callID, "deny")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("AC-3: POST confirmations status = %d, want 202; body: %s", rr.Code, rr.Body.String())
	}

	// Wait for the turn to complete after denial.
	waitForCompleted(t, bus, 0, 5*time.Second)

	// AC-3a: tool NOT executed.
	if len(tracker.snapshot()) != 0 {
		t.Error("AC-3: tool executed after denial — should not execute")
	}

	// AC-3b: synthetic "declined" tool-result persisted.
	if !hasToolResult(t, s, p, convID, callID) {
		t.Error("AC-3: no synthetic tool-result persisted for denied call")
	}

	// AC-3c: tool.failed emitted with the callID.
	evs := bus.snapshot()
	var toolFailedForCall bool
	for _, e := range evs {
		if e.event.Type == "tool.failed" {
			if pl, ok := e.event.Data.(harness.ToolFailedPayload); ok && pl.CallID == callID {
				toolFailedForCall = true
			}
		}
	}
	if !toolFailedForCall {
		t.Error("AC-3: tool.failed not emitted for denied call")
	}

	// AC-3d: turn still completes (model gets one more round to acknowledge).
	if countEventType(evs, "message.completed") != 1 {
		t.Errorf("AC-3: message.completed count = %d, want 1", countEventType(evs, "message.completed"))
	}

	// AC-3e: confirmation row status updated to denied.
	st := getPendingConfirmationStatus(t, s, p.UserID, callID)
	if st != "denied" {
		t.Errorf("AC-3: row status = %q, want denied", st)
	}
}

// ── AC-4: Resume correctness — persisted state drives re-entry identically ──

// TestConfirm_AC4_ResumeCorrectness verifies that constructing a conversation
// with a dangling tool call (pending_confirmation row exists) in the DB and calling
// Resolve reconstitutes the turn identically to a freshly-suspended turn.
func TestConfirm_AC4_ResumeCorrectness(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := &callCapture{}

	const callID = "call_confirm_ac4"
	extTool := makeConfirmTool("ext_tool", tracker)

	// The model will be called once (re-entry after resume) to give the final reply.
	round2 := []gateway.Chunk{
		{TextDelta: "resumed and done"},
		{Done: true},
	}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round2}}
	h := buildHarnessWithConfirmTool(t, s, gw, bus, extTool, time.Hour)
	srv := buildConfirmServer(h)

	// Manually construct the conversation state: user message, assistant message
	// with tool_calls, and a pending_confirmations row — as if a previous turn suspended.
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
		Content:        "please do the external thing",
	}); err != nil {
		t.Fatalf("InsertMessage user: %v", err)
	}

	// Persist assistant message with tool_calls (dangling — no result yet).
	toolCallsJSON, _ := json.Marshal([]gateway.ToolCall{{
		ID:        callID,
		Name:      "ext_tool",
		Arguments: json.RawMessage(`{"call_id":"ac4"}`),
	}})
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

	// Persist the pending_confirmations row (as the real requestConfirmation would).
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	_, err := s.Writer().ExecContext(context.Background(),
		`INSERT INTO pending_confirmations (id, conversation_id, message_id, user_id, tool_name, args, risk, status, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
		callID, convID, assistantMsgID, p.UserID, "ext_tool",
		`{"call_id":"ac4"}`, "external", expiresAt,
	)
	if err != nil {
		t.Fatalf("insert pending_confirmations: %v", err)
	}

	// Now call Resolve(approve) — this should re-enter run, execute the tool,
	// and continue to message.completed.
	rr := resolveViaHTTP(t, srv, p, convID, callID, "approve")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("AC-4: POST confirmations status = %d, want 202; body: %s", rr.Code, rr.Body.String())
	}

	// Wait for turn to complete.
	waitForCompleted(t, bus, 1, 5*time.Second)

	// AC-4a: tool was executed.
	if len(tracker.snapshot()) == 0 {
		t.Error("AC-4: tool not executed after resume from persisted state")
	}

	// AC-4b: tool-result persisted.
	if !hasToolResult(t, s, p, convID, callID) {
		t.Error("AC-4: tool-result not persisted after resume")
	}

	// AC-4c: turn completed.
	evs := bus.snapshot()
	if countEventType(evs, "message.completed") != 1 {
		t.Errorf("AC-4: message.completed count = %d, want 1", countEventType(evs, "message.completed"))
	}

	// AC-4d: only one tool.started (the approved tool, not the gateway round that
	// was already consumed before suspension).
	toolStartedCount := countEventType(evs, "tool.started")
	if toolStartedCount != 1 {
		t.Errorf("AC-4: tool.started count = %d, want 1 (only the approved tool)", toolStartedCount)
	}
}

// ── AC-5: Three Confirm-tier calls → three rows + three events ────────────────

func TestConfirm_AC5_ThreeCallsThreeConfirmations(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := &callCapture{}

	const (
		callID1 = "call_c5_a"
		callID2 = "call_c5_b"
		callID3 = "call_c5_c"
	)
	extTool := makeConfirmTool("ext_tool", tracker)

	// Round 1: three Confirm-tier calls in a single round.
	round1 := []gateway.Chunk{
		toolCallChunk(callID1, "ext_tool", `{"call_id":"c5a"}`),
		toolCallChunk(callID2, "ext_tool", `{"call_id":"c5b"}`),
		toolCallChunk(callID3, "ext_tool", `{"call_id":"c5c"}`),
		{Done: true},
	}
	// After all three are approved, the model gives a final reply.
	roundFinal := []gateway.Chunk{
		{TextDelta: "all done"},
		{Done: true},
	}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1, roundFinal}}
	h := buildHarnessWithConfirmTool(t, s, gw, bus, extTool, time.Hour)
	srv := buildConfirmServer(h)

	convID := id.New()

	// Fire the turn; it should suspend with three confirmation.required events.
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
	if err := h.StartTurn(context.Background(), convID, harness.InboundMessage{Content: "test"}, origin, p); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}

	// Wait for all three confirmation.required events.
	var evs []fakeEvent
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		evs = bus.snapshot()
		if countEventType(evs, "confirmation.required") >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// AC-5a: three confirmation.required events.
	if countEventType(evs, "confirmation.required") != 3 {
		t.Errorf("AC-5: confirmation.required count = %d, want 3", countEventType(evs, "confirmation.required"))
	}

	// AC-5b: three separate pending_confirmations rows.
	for _, cid := range []string{callID1, callID2, callID3} {
		if !hasPendingConfirmation(t, s, p.UserID, cid) {
			t.Errorf("AC-5: no pending_confirmations row for callID %s", cid)
		}
	}

	// AC-5c: no terminal event yet.
	if hasTerminalEvent(evs) {
		t.Error("AC-5: terminal event emitted with pending confirmations — should not")
	}

	// Now resolve them one by one. After each of the first two, the turn should
	// re-enter, execute that one, and suspend again. After the third, it should complete.

	// Approve call 1 — turn re-enters, executes call1, sees call2+call3 still pending, suspends again.
	rr1 := resolveViaHTTP(t, srv, p, convID, callID1, "approve")
	if rr1.Code != http.StatusAccepted {
		t.Fatalf("AC-5: approve call1 status = %d, want 202", rr1.Code)
	}

	// Wait for call2's confirmation.required to be re-emitted (or just the tool.started for call1).
	time.Sleep(200 * time.Millisecond)

	// Tool executed for call1 but turn still suspended (call2 & call3 pending).
	if !hasToolResult(t, s, p, convID, callID1) {
		t.Error("AC-5: tool-result not persisted for call1 after approval")
	}
	evs2 := bus.snapshot()
	if hasTerminalEvent(evs2) {
		t.Error("AC-5: terminal event after approving only one of three — should still be suspended")
	}

	// Approve call 2.
	rr2 := resolveViaHTTP(t, srv, p, convID, callID2, "approve")
	if rr2.Code != http.StatusAccepted {
		t.Fatalf("AC-5: approve call2 status = %d, want 202", rr2.Code)
	}
	time.Sleep(200 * time.Millisecond)

	if !hasToolResult(t, s, p, convID, callID2) {
		t.Error("AC-5: tool-result not persisted for call2 after approval")
	}
	evs3 := bus.snapshot()
	if hasTerminalEvent(evs3) {
		t.Error("AC-5: terminal event after approving two of three — should still be suspended")
	}

	// Approve call 3 — all three done, loop continues to final reply.
	rr3 := resolveViaHTTP(t, srv, p, convID, callID3, "approve")
	if rr3.Code != http.StatusAccepted {
		t.Fatalf("AC-5: approve call3 status = %d, want 202", rr3.Code)
	}
	waitForCompleted(t, bus, 3, 5*time.Second)

	if !hasToolResult(t, s, p, convID, callID3) {
		t.Error("AC-5: tool-result not persisted for call3 after approval")
	}
	if len(tracker.snapshot()) != 3 {
		t.Errorf("AC-5: tool executed %d times, want 3", len(tracker.snapshot()))
	}

	evsFinal := bus.snapshot()
	if countEventType(evsFinal, "message.completed") != 1 {
		t.Errorf("AC-5: message.completed count = %d, want 1", countEventType(evsFinal, "message.completed"))
	}
}

// ── AC-6: HTTP endpoint returns 202, 404 for unknown, 400 for bad body ────────

func TestConfirm_AC6_HTTPEndpointStatusCodes(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := &callCapture{}

	const callID = "call_confirm_ac6"
	extTool := makeConfirmTool("ext_tool", tracker)

	round1 := []gateway.Chunk{
		toolCallChunk(callID, "ext_tool", `{"call_id":"ac6"}`),
		{Done: true},
	}
	round2 := []gateway.Chunk{{TextDelta: "ok"}, {Done: true}}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1, round2}}
	h := buildHarnessWithConfirmTool(t, s, gw, bus, extTool, time.Hour)
	srv := buildConfirmServer(h)

	convID := id.New()
	_ = startTurnExpectSuspend(t, h, s, p, bus, convID)

	// AC-6a: 400 for malformed body.
	req400 := makeAuthedRequest(
		http.MethodPost,
		"/api/v1/conversations/"+convID+"/confirmations/"+callID,
		[]byte(`{bad json`),
		p,
	)
	rr400 := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rr400, req400)
	if rr400.Code != http.StatusBadRequest {
		t.Errorf("AC-6: malformed body status = %d, want 400", rr400.Code)
	}

	// AC-6b: 422 for missing/invalid decision value.
	badDecision, _ := json.Marshal(map[string]string{"decision": "maybe"})
	reqBad := makeAuthedRequest(
		http.MethodPost,
		"/api/v1/conversations/"+convID+"/confirmations/"+callID,
		badDecision,
		p,
	)
	rrBad := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rrBad, reqBad)
	if rrBad.Code != http.StatusUnprocessableEntity {
		t.Errorf("AC-6: invalid decision status = %d, want 422", rrBad.Code)
	}

	// AC-6c: 404 for unknown callId (not the user's or doesn't exist).
	p2 := makeTestUser(t, s)
	reqForeign := makeAuthedRequest(
		http.MethodPost,
		"/api/v1/conversations/"+convID+"/confirmations/nonexistent-call-id",
		[]byte(`{"decision":"approve"}`),
		p2, // different user
	)
	rrForeign := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rrForeign, reqForeign)
	if rrForeign.Code != http.StatusNotFound {
		t.Errorf("AC-6: foreign user / unknown callId status = %d, want 404", rrForeign.Code)
	}

	// AC-6d: 202 for a valid approve decision.
	rrOK := resolveViaHTTP(t, srv, p, convID, callID, "approve")
	if rrOK.Code != http.StatusAccepted {
		t.Errorf("AC-6: valid approve status = %d, want 202; body: %s", rrOK.Code, rrOK.Body.String())
	}

	// AC-6e: 409 for attempting to resolve an already-resolved callId.
	waitForCompleted(t, bus, 1, 3*time.Second)
	rr409 := resolveViaHTTP(t, srv, p, convID, callID, "deny")
	if rr409.Code != http.StatusConflict {
		t.Errorf("AC-6: already-resolved callId status = %d, want 409", rr409.Code)
	}
}

// ── Config.ConfirmationTTL default ───────────────────────────────────────────

// TestConfirm_DefaultTTL verifies that a zero-value ConfirmationTTL uses the default (24h),
// and that ExpiresAt is set in the persisted row.
func TestConfirm_DefaultTTL(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := &callCapture{}

	const callID = "call_confirm_ttl"
	extTool := makeConfirmTool("ext_tool", tracker)

	round1 := []gateway.Chunk{
		toolCallChunk(callID, "ext_tool", `{"call_id":"ttl"}`),
		{Done: true},
	}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1}}
	// Zero Config — default TTL should apply (24h).
	reg := capability.NewRegistry()
	if err := reg.Add(fakeSource{tools: []capability.Tool{extTool}}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}
	h := harness.New(reg, gw, s, bus, harness.Config{}, harness.NewDiscardLogger())

	convID := id.New()
	_ = startTurnExpectSuspend(t, h, s, p, bus, convID)

	// Confirm the row exists.
	if !hasPendingConfirmation(t, s, p.UserID, callID) {
		t.Fatal("no pending_confirmations row for default TTL test")
	}

	// The ExpiresAt should be approximately now+24h.
	var expiresAtStr string
	err := s.Reader().QueryRowContext(
		context.Background(),
		`SELECT expires_at FROM pending_confirmations WHERE id = ? AND user_id = ?`,
		callID, p.UserID,
	).Scan(&expiresAtStr)
	if err != nil {
		t.Fatalf("query expires_at: %v", err)
	}
	expiresAt, err := time.Parse(time.RFC3339, expiresAtStr)
	if err != nil {
		t.Fatalf("parse expires_at %q: %v", expiresAtStr, err)
	}

	// Should be ~24h from now (allow 5 minutes of slack).
	expectedMin := time.Now().UTC().Add(23*time.Hour + 55*time.Minute)
	expectedMax := time.Now().UTC().Add(24*time.Hour + 5*time.Minute)
	if expiresAt.Before(expectedMin) || expiresAt.After(expectedMax) {
		t.Errorf("ExpiresAt = %v, want approximately now+24h", expiresAt)
	}
}

// ── Race-clean ────────────────────────────────────────────────────────────────

// TestConfirm_RaceClean exercises the suspend/resume flow under -race.
func TestConfirm_RaceClean(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := &callCapture{}

	const callID = "call_confirm_race"
	extTool := makeConfirmTool("ext_tool", tracker)

	round1 := []gateway.Chunk{
		toolCallChunk(callID, "ext_tool", `{"call_id":"race"}`),
		{Done: true},
	}
	round2 := []gateway.Chunk{{TextDelta: "ok"}, {Done: true}}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1, round2}}
	h := buildHarnessWithConfirmTool(t, s, gw, bus, extTool, time.Hour)
	srv := buildConfirmServer(h)

	convID := id.New()
	_ = startTurnExpectSuspend(t, h, s, p, bus, convID)

	rr := resolveViaHTTP(t, srv, p, convID, callID, "approve")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("approve status = %d, want 202", rr.Code)
	}

	waitForCompleted(t, bus, 1, 5*time.Second)

	if countEventType(bus.snapshot(), "message.completed") != 1 {
		t.Error("message.completed not emitted after race-clean approve")
	}
}

// ── Scope isolation: different user cannot resolve another user's confirmation ─

func TestConfirm_ScopeIsolation_CrossUserResolveRejected(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	pA := makeTestUser(t, s)
	pB := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := &callCapture{}

	const callID = "call_confirm_scope"
	extTool := makeConfirmTool("ext_tool", tracker)

	round1 := []gateway.Chunk{
		toolCallChunk(callID, "ext_tool", `{"call_id":"scope"}`),
		{Done: true},
	}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1}}
	h := buildHarnessWithConfirmTool(t, s, gw, bus, extTool, time.Hour)
	srv := buildConfirmServer(h)

	convID := id.New()
	_ = startTurnExpectSuspend(t, h, s, pA, bus, convID)

	// User B tries to approve user A's confirmation — must get 404.
	body, _ := json.Marshal(map[string]string{"decision": "approve"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/confirmations/"+callID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := httpx.ContextWithPrincipal(req.Context(), pB)
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("cross-user resolve status = %d, want 404", rr.Code)
	}

	// Confirm pA's row is still pending.
	if getPendingConfirmationStatus(t, s, pA.UserID, callID) != "pending" {
		t.Error("cross-user resolve changed pA's row status — scope isolation broken")
	}

	// Confirm pA's tool was not executed.
	if len(tracker.snapshot()) != 0 {
		t.Error("cross-user resolve executed pA's tool — scope isolation broken")
	}

	// Avoid leaking the goroutine: just verify the row is still pending and no turn.failed.
	evs := bus.snapshot()
	for _, e := range evs {
		if e.event.Type == "turn.failed" {
			t.Error("turn.failed emitted on cross-user reject — should not")
		}
	}
	_ = sql.ErrNoRows // ensure import used
}

// ── SHOULD-FIX 4: callID must belong to the path {id} conversation ───────────

// TestConfirm_ConvIDOwnership_WrongConv verifies that POSTing to
// /conversations/AAA/confirmations/callX returns 404 when callX actually belongs
// to conversation BBB (same user). The {id} segment must not be decorative.
func TestConfirm_ConvIDOwnership_WrongConv(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := &callCapture{}

	const callID = "call_wrong_conv"
	extTool := makeConfirmTool("ext_tool", tracker)

	round1 := []gateway.Chunk{
		toolCallChunk(callID, "ext_tool", `{"call_id":"wc"}`),
		{Done: true},
	}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1}}
	h := buildHarnessWithConfirmTool(t, s, gw, bus, extTool, time.Hour)
	srv := buildConfirmServer(h)

	// Conversation A: owns callID.
	convA := id.New()
	_ = startTurnExpectSuspend(t, h, s, p, bus, convA)

	if !hasPendingConfirmation(t, s, p.UserID, callID) {
		t.Fatal("no pending_confirmations row created for callID")
	}

	// Attempt to resolve callID via a different conversation path (convB).
	convB := id.New()
	body, _ := json.Marshal(map[string]string{"decision": "approve"})
	req := makeAuthedRequest(
		http.MethodPost,
		"/api/v1/conversations/"+convB+"/confirmations/"+callID,
		body,
		p,
	)
	rr := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rr, req)

	// Must be 404 — the callID belongs to convA, not convB.
	if rr.Code != http.StatusNotFound {
		t.Errorf("wrong-conv resolve status = %d, want 404; body: %s", rr.Code, rr.Body.String())
	}

	// The row must still be pending (not resolved by the wrong-conv request).
	if st := getPendingConfirmationStatus(t, s, p.UserID, callID); st != "pending" {
		t.Errorf("callID row status = %q after wrong-conv resolve, want pending", st)
	}

	// Tool must not have been executed.
	if len(tracker.snapshot()) != 0 {
		t.Error("tool executed after wrong-conv resolve — must not execute")
	}
}
