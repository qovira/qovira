package harness_test

// expiry_test.go — TDD tests for the "Expire stale confirmations" slice.
//
// Acceptance criteria covered:
//   (1) Lazy-expiry: approve/deny arriving past ExpiresAt is rejected as expired — row transitions pending→expired,
//       tool NOT executed, assistant message abandoned, confirmation.expired emitted, NO model round, HTTP 409 with
//       code "confirmation_expired".
//   (2) SweepExpiredConfirmations: marks lapsed pending rows expired, emits confirmation.expired per row, abandons
//       messages, returns correct count, does NOT touch non-lapsed rows.
//   (3) Abandoned turns are inert — conversation is never treated as resumable after expiry; no model round fires.
//   (4) Concurrency: SweepExpiredConfirmations runs concurrently with Resolve of the same row; exactly one winner; no
//       double event. Hammered under -count=50.

import (
	"context"
	"encoding/json"
	"net/http"
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

// ── helpers ───────────────────────────────────────────────────────────────────

// buildHarnessWithClockAndTool builds a Harness with an injected clock and a single tool, so expiry tests can set
// ExpiresAt in the past deterministically.
func buildHarnessWithClockAndTool(
	t *testing.T,
	s *store.Store,
	gw harness.Chatter,
	bus events.Publisher,
	tool capability.Tool,
	nowFn func() time.Time,
) *harness.Harness {
	t.Helper()
	reg := capability.NewRegistry()
	if tool.Name != "" {
		if err := reg.Add(fakeSource{tools: []capability.Tool{tool}}); err != nil {
			t.Fatalf("registry.Add: %v", err)
		}
	}
	cfg := harness.Config{ConfirmationTTL: time.Hour}
	return harness.NewWithClock(reg, gw, s, bus, cfg, harness.NewDiscardLogger(), nowFn)
}

// insertSuspendedConversation manually constructs a conversation in a suspended state: user message + assistant
// message with one Confirm-tier tool call + pending_confirmations row. Returns the callID, assistantMsgID, and convID.
// The expires_at on the pending row is set to the provided expiresAt string.
func insertSuspendedConversation(
	t *testing.T,
	s *store.Store,
	p store.Principal,
	toolName string,
	expiresAt string,
) (convID, callID, assistantMsgID string) {
	t.Helper()
	convID = id.New()
	callID = id.New()
	scope := store.UserScope(p)
	sq := s.ForUser(scope)

	if err := sq.UpsertConversation(context.Background(), convID); err != nil {
		t.Fatalf("UpsertConversation: %v", err)
	}
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             id.New(),
		ConversationID: convID,
		Role:           "user",
		Content:        "please do the thing",
	}); err != nil {
		t.Fatalf("InsertMessage user: %v", err)
	}

	toolCallsJSON, _ := json.Marshal([]gateway.ToolCall{{
		ID:        callID,
		Name:      toolName,
		Arguments: json.RawMessage(`{"call_id":"test"}`),
	}})
	assistantMsgID = id.New()
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             assistantMsgID,
		ConversationID: convID,
		Role:           "assistant",
		Content:        "",
		ToolCalls:      string(toolCallsJSON),
	}); err != nil {
		t.Fatalf("InsertMessage assistant: %v", err)
	}

	if _, err := s.Writer().ExecContext(context.Background(),
		`INSERT INTO pending_confirmations
		 (id, conversation_id, message_id, user_id, tool_name, args, risk, status, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
		callID, convID, assistantMsgID, p.UserID, toolName,
		`{"call_id":"test"}`, "external", expiresAt,
	); err != nil {
		t.Fatalf("insert pending_confirmations: %v", err)
	}
	return convID, callID, assistantMsgID
}

// getPendingConfirmationStatus returns the status of a pending_confirmations row by direct DB query.
func getConfirmationStatus(t *testing.T, s *store.Store, callID, userID string) string {
	t.Helper()
	var status string
	if err := s.Reader().QueryRowContext(context.Background(),
		`SELECT status FROM pending_confirmations WHERE id = ? AND user_id = ?`,
		callID, userID,
	).Scan(&status); err != nil {
		t.Fatalf("getConfirmationStatus: %v", err)
	}
	return status
}

// isMessageAbandoned returns true if the message's abandoned column is 1.
func isMessageAbandoned(t *testing.T, s *store.Store, msgID, userID string) bool {
	t.Helper()
	var abandoned int64
	if err := s.Reader().QueryRowContext(context.Background(),
		`SELECT abandoned FROM messages WHERE id = ? AND user_id = ?`,
		msgID, userID,
	).Scan(&abandoned); err != nil {
		t.Fatalf("isMessageAbandoned: %v", err)
	}
	return abandoned != 0
}

// hasExpiredEvent returns true if a "confirmation.expired" event for callID appears in evs.
func hasExpiredEvent(evs []fakeEvent, callID string) bool {
	for _, e := range evs {
		if e.event.Type != "confirmation.expired" {
			continue
		}
		if pl, ok := e.event.Data.(harness.ConfirmationExpiredPayload); ok && pl.CallID == callID {
			return true
		}
	}
	return false
}

// countExpiredEvents counts distinct "confirmation.expired" events for callID.
func countExpiredEventsFor(evs []fakeEvent, callID string) int {
	n := 0
	for _, e := range evs {
		if e.event.Type != "confirmation.expired" {
			continue
		}
		if pl, ok := e.event.Data.(harness.ConfirmationExpiredPayload); ok && pl.CallID == callID {
			n++
		}
	}
	return n
}

// ── AC-1: Lazy expiry — approve past ExpiresAt ────────────────────────────────

// TestExpiry_LazyCheck_ApprovePastTTLIsRejected verifies that an approve arriving after ExpiresAt is rejected with
// 409/confirmation_expired. The row transitions to "expired", the tool does NOT execute, the assistant message is
// marked abandoned, confirmation.expired is emitted, and no model round fires.
func TestExpiry_LazyCheck_ApprovePastTTLIsRejected(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := newCountingTool()

	const toolName = "expiry_lazy_ext_tool"
	extTool := capability.NewTool(
		toolName, "expiry lazy test tool",
		json.RawMessage(`{"type":"object","properties":{"call_id":{"type":"string"}}}`),
		capability.RiskExternal,
		func(_ context.Context, _ store.Scope, _ struct {
			CallID string `json:"call_id"`
		}) (capability.Result, error) {
			tracker.record("executed")
			return map[string]string{"result": "done"}, nil
		},
	)

	// Clock starts in the past so ExpiresAt is already lapsed.
	pastTime := time.Now().UTC().Add(-2 * time.Hour)
	nowFn := func() time.Time { return pastTime }

	// Gateway will NOT be called for the expired path; if it is, the test will see an extra event.
	gw := &queuedChatter{rounds: [][]gateway.Chunk{{
		{TextDelta: "should not be called"},
		{Done: true},
	}}}
	h := buildHarnessWithClockAndTool(t, s, gw, bus, extTool, nowFn)
	srv := buildConfirmServer(h)

	// Insert a conversation with an already-lapsed expires_at (1 hour ago).
	pastExpiry := pastTime.Add(-time.Hour).Format(time.RFC3339)
	convID, callID, assistantMsgID := insertSuspendedConversation(t, s, p, toolName, pastExpiry)

	// Attempt to approve — should be rejected as expired.
	rr := resolveViaHTTP(t, srv, p, convID, callID, "approve")

	// AC-1a: HTTP 409 with code "confirmation_expired".
	if rr.Code != http.StatusConflict {
		t.Errorf("AC-1: approve past TTL status = %d, want 409; body: %s", rr.Code, rr.Body.String())
	}
	// The response body should carry the confirmation_expired code.
	body := rr.Body.String()
	if !strings.Contains(body, "confirmation_expired") {
		t.Errorf("AC-1: response body does not contain 'confirmation_expired'; body: %s", body)
	}

	// Give the goroutine a moment to settle.
	time.Sleep(50 * time.Millisecond)

	// AC-1b: row status = "expired".
	if st := getConfirmationStatus(t, s, callID, p.UserID); st != "expired" {
		t.Errorf("AC-1: row status = %q, want 'expired'", st)
	}

	// AC-1c: tool was NOT executed.
	if n := tracker.countFor("executed"); n != 0 {
		t.Errorf("AC-1: tool executed %d times, want 0 (must not execute on expiry)", n)
	}

	// AC-1d: assistant message is abandoned.
	if !isMessageAbandoned(t, s, assistantMsgID, p.UserID) {
		t.Error("AC-1: assistant message is NOT abandoned after expiry")
	}

	// AC-1e: confirmation.expired emitted.
	evs := bus.snapshot()
	if !hasExpiredEvent(evs, callID) {
		t.Errorf("AC-1: confirmation.expired not emitted for callID %s; events: %v", callID, evs)
	}

	// AC-1f: no model round (no message.completed, no turn.failed).
	if n := countEventType(evs, "message.completed"); n != 0 {
		t.Errorf("AC-1: message.completed emitted %d times, want 0 (no model round on expiry)", n)
	}
	if n := countEventType(evs, "turn.failed"); n != 0 {
		t.Errorf("AC-1: turn.failed emitted %d times, want 0 (no model round on expiry)", n)
	}

	// AC-1g: gateway NOT called.
	if gw.pos != 0 {
		t.Errorf("AC-1: gateway called %d times, want 0 (no model round on expiry)", gw.pos)
	}
}

// TestExpiry_LazyCheck_DenyPastTTLIsRejected verifies that a deny (not just approve) arriving after ExpiresAt is also
// rejected as expired.
func TestExpiry_LazyCheck_DenyPastTTLIsRejected(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	const toolName = "expiry_deny_ext_tool"
	extTool := capability.NewTool(
		toolName, "expiry deny test tool",
		json.RawMessage(`{"type":"object","properties":{"call_id":{"type":"string"}}}`),
		capability.RiskExternal,
		func(_ context.Context, _ store.Scope, _ struct {
			CallID string `json:"call_id"`
		}) (capability.Result, error) {
			return map[string]string{"result": "done"}, nil
		},
	)

	pastTime := time.Now().UTC().Add(-2 * time.Hour)
	nowFn := func() time.Time { return pastTime }
	gw := &queuedChatter{rounds: [][]gateway.Chunk{{
		{TextDelta: "should not be called"},
		{Done: true},
	}}}
	h := buildHarnessWithClockAndTool(t, s, gw, bus, extTool, nowFn)
	srv := buildConfirmServer(h)

	pastExpiry := pastTime.Add(-time.Hour).Format(time.RFC3339)
	convID, callID, _ := insertSuspendedConversation(t, s, p, toolName, pastExpiry)

	rr := resolveViaHTTP(t, srv, p, convID, callID, "deny")
	if rr.Code != http.StatusConflict {
		t.Errorf("AC-1 (deny): status = %d, want 409; body: %s", rr.Code, rr.Body.String())
	}

	time.Sleep(50 * time.Millisecond)

	if st := getConfirmationStatus(t, s, callID, p.UserID); st != "expired" {
		t.Errorf("AC-1 (deny): row status = %q, want 'expired'", st)
	}
	if !hasExpiredEvent(bus.snapshot(), callID) {
		t.Error("AC-1 (deny): confirmation.expired not emitted")
	}
	if gw.pos != 0 {
		t.Errorf("AC-1 (deny): gateway called %d times, want 0", gw.pos)
	}
}

// TestExpiry_LazyCheck_NotExpiredStillWorks verifies that an approve arriving BEFORE ExpiresAt still works normally
// (regression guard).
func TestExpiry_LazyCheck_NotExpiredStillWorks(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := newCountingTool()

	const toolName = "expiry_valid_ext_tool"
	extTool := capability.NewTool(
		toolName, "expiry valid test tool",
		json.RawMessage(`{"type":"object","properties":{"call_id":{"type":"string"}}}`),
		capability.RiskExternal,
		func(_ context.Context, _ store.Scope, _ struct {
			CallID string `json:"call_id"`
		}) (capability.Result, error) {
			tracker.record("executed")
			return map[string]string{"result": "done"}, nil
		},
	)

	// Clock is "now" — confirmation is still valid.
	currentTime := time.Now().UTC()
	nowFn := func() time.Time { return currentTime }

	round2 := []gateway.Chunk{{TextDelta: "done"}, {Done: true}}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round2}}
	h := buildHarnessWithClockAndTool(t, s, gw, bus, extTool, nowFn)
	srv := buildConfirmServer(h)

	// expires_at is 1h from "now" — still valid.
	futureExpiry := currentTime.Add(time.Hour).Format(time.RFC3339)
	convID, callID, _ := insertSuspendedConversation(t, s, p, toolName, futureExpiry)

	rr := resolveViaHTTP(t, srv, p, convID, callID, "approve")
	if rr.Code != http.StatusAccepted {
		t.Errorf("not-expired approve: status = %d, want 202; body: %s", rr.Code, rr.Body.String())
	}

	waitForCompleted(t, bus, 1, 5*time.Second)

	// Tool was executed.
	if n := tracker.countFor("executed"); n != 1 {
		t.Errorf("not-expired approve: tool executed %d times, want 1", n)
	}
	// Row status = approved.
	if st := getConfirmationStatus(t, s, callID, p.UserID); st != "approved" {
		t.Errorf("not-expired approve: row status = %q, want 'approved'", st)
	}
	// No confirmation.expired event.
	if hasExpiredEvent(bus.snapshot(), callID) {
		t.Error("not-expired approve: confirmation.expired emitted — should not be")
	}
}

// ── AC-2: SweepExpiredConfirmations ──────────────────────────────────────────

// TestExpiry_Sweep_MarksLapsedRowsAndEmitsEvents verifies that SweepExpiredConfirmations marks all lapsed pending rows
// as expired, emits one confirmation.expired per row on the correct user bus, abandons their assistant messages,
// returns the correct count, and leaves non-lapsed rows untouched.
func TestExpiry_Sweep_MarksLapsedRowsAndEmitsEvents(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	// Two users, several lapsed rows per user, one non-lapsed row.
	pA := makeTestUser(t, s)
	pB := makeTestUser(t, s)
	bus := &fakeBus{}

	const toolName = "sweep_ext_tool"
	extTool := capability.NewTool(
		toolName, "sweep test tool",
		json.RawMessage(`{"type":"object","properties":{"call_id":{"type":"string"}}}`),
		capability.RiskExternal,
		func(_ context.Context, _ store.Scope, _ struct {
			CallID string `json:"call_id"`
		}) (capability.Result, error) {
			return map[string]string{"result": "done"}, nil
		},
	)

	// All "past" confirmations have expiresAt 2h ago.
	past := time.Now().UTC().Add(-2 * time.Hour)
	pastExpiry := past.Format(time.RFC3339)

	// Insert 2 lapsed rows for user A.
	convA1, callA1, msgA1 := insertSuspendedConversation(t, s, pA, toolName, pastExpiry)
	convA2, callA2, msgA2 := insertSuspendedConversation(t, s, pA, toolName, pastExpiry)
	_ = convA1
	_ = convA2

	// Insert 1 lapsed row for user B.
	convB1, callB1, msgB1 := insertSuspendedConversation(t, s, pB, toolName, pastExpiry)
	_ = convB1

	// Insert 1 NON-lapsed row for user A (expires 1h from now).
	futureExpiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	_, callAFuture, _ := insertSuspendedConversation(t, s, pA, toolName, futureExpiry)

	// Build a harness with a "now" clock that sees all past rows as lapsed.
	nowFn := func() time.Time { return time.Now().UTC() }
	gw := &queuedChatter{rounds: [][]gateway.Chunk{{{Done: true}}}}
	h := buildHarnessWithClockAndTool(t, s, gw, bus, extTool, nowFn)

	n, err := h.SweepExpiredConfirmations(context.Background())
	if err != nil {
		t.Fatalf("SweepExpiredConfirmations: %v", err)
	}

	// AC-2a: count = 3 (callA1, callA2, callB1).
	if n != 3 {
		t.Errorf("AC-2: swept count = %d, want 3", n)
	}

	// AC-2b: all lapsed rows → expired.
	for _, tc := range []struct {
		callID string
		userID string
	}{
		{callA1, pA.UserID},
		{callA2, pA.UserID},
		{callB1, pB.UserID},
	} {
		if st := getConfirmationStatus(t, s, tc.callID, tc.userID); st != "expired" {
			t.Errorf("AC-2: %s status = %q, want 'expired'", tc.callID, st)
		}
	}

	// AC-2c: non-lapsed row untouched (still "pending").
	if st := getConfirmationStatus(t, s, callAFuture, pA.UserID); st != "pending" {
		t.Errorf("AC-2: non-lapsed row %s status = %q, want 'pending'", callAFuture, st)
	}

	// AC-2d: confirmation.expired emitted for each lapsed row on the correct user.
	evs := bus.snapshot()
	for _, callID := range []string{callA1, callA2} {
		if !hasExpiredEvent(evs, callID) {
			t.Errorf("AC-2: confirmation.expired not emitted for userA callID %s", callID)
		}
	}
	if !hasExpiredEvent(evs, callB1) {
		t.Errorf("AC-2: confirmation.expired not emitted for userB callID %s", callB1)
	}

	// Verify events emitted to the right user by checking the bus.
	for _, e := range evs {
		if e.event.Type != "confirmation.expired" {
			continue
		}
		pl, ok := e.event.Data.(harness.ConfirmationExpiredPayload)
		if !ok {
			continue
		}
		switch pl.CallID {
		case callA1, callA2:
			if e.userID != pA.UserID {
				t.Errorf("AC-2: event for callA published to userID %q, want %q", e.userID, pA.UserID)
			}
		case callB1:
			if e.userID != pB.UserID {
				t.Errorf("AC-2: event for callB published to userID %q, want %q", e.userID, pB.UserID)
			}
		}
	}

	// AC-2e: assistant messages abandoned.
	for _, tc := range []struct {
		msgID  string
		userID string
	}{
		{msgA1, pA.UserID},
		{msgA2, pA.UserID},
		{msgB1, pB.UserID},
	} {
		if !isMessageAbandoned(t, s, tc.msgID, tc.userID) {
			t.Errorf("AC-2: message %s not abandoned", tc.msgID)
		}
	}

	// AC-2f: no terminal event (no model round fired by sweep).
	if n := countEventType(evs, "message.completed"); n != 0 {
		t.Errorf("AC-2: message.completed emitted %d times by sweep — should be 0", n)
	}
}

// ── AC-3: Abandoned turns are inert ──────────────────────────────────────────

// TestExpiry_AbandonedTurnIsInert verifies that after expiry, the conversation is not treated as resumable:
// outstandingToolCalls returns nothing, isTurnComplete returns true, and calling Resolve (after the row is expired)
// returns 409 without spawning a model round.
func TestExpiry_AbandonedTurnIsInert(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := newCountingTool()

	const toolName = "abandoned_ext_tool"
	extTool := capability.NewTool(
		toolName, "abandoned test tool",
		json.RawMessage(`{"type":"object","properties":{"call_id":{"type":"string"}}}`),
		capability.RiskExternal,
		func(_ context.Context, _ store.Scope, _ struct {
			CallID string `json:"call_id"`
		}) (capability.Result, error) {
			tracker.record("executed")
			return map[string]string{"result": "done"}, nil
		},
	)

	// Clock: expiry already passed.
	pastTime := time.Now().UTC().Add(-2 * time.Hour)
	nowFn := func() time.Time { return pastTime }

	// Gateway returns a final reply. If the abandoned turn is not inert, this gets called.
	gw := &queuedChatter{rounds: [][]gateway.Chunk{{
		{TextDelta: "should not be called"},
		{Done: true},
	}}}
	h := buildHarnessWithClockAndTool(t, s, gw, bus, extTool, nowFn)
	srv := buildConfirmServer(h)

	pastExpiry := pastTime.Add(-time.Hour).Format(time.RFC3339)
	convID, callID, _ := insertSuspendedConversation(t, s, p, toolName, pastExpiry)

	// First Resolve: lazy expiry triggers, row → expired, message abandoned.
	rr1 := resolveViaHTTP(t, srv, p, convID, callID, "approve")
	if rr1.Code != http.StatusConflict {
		t.Fatalf("AC-3: first resolve status = %d, want 409", rr1.Code)
	}

	time.Sleep(50 * time.Millisecond)

	// Second Resolve on the same expired row: must also return 409 (already resolved), NOT 500, and must NOT fire a
	// model round.
	rr2 := resolveViaHTTP(t, srv, p, convID, callID, "approve")
	if rr2.Code != http.StatusConflict {
		t.Errorf("AC-3: second resolve status = %d, want 409 (already expired/resolved)", rr2.Code)
	}

	time.Sleep(50 * time.Millisecond)

	// Tool must NEVER have executed.
	if n := tracker.countFor("executed"); n != 0 {
		t.Errorf("AC-3: tool executed %d times after expiry, want 0", n)
	}

	// Gateway must NOT have been called.
	if gw.pos != 0 {
		t.Errorf("AC-3: gateway called %d times after expiry, want 0", gw.pos)
	}

	// No terminal event from turn re-entry.
	evs := bus.snapshot()
	if n := countEventType(evs, "message.completed"); n != 0 {
		t.Errorf("AC-3: message.completed emitted %d times, want 0 (abandoned turn inert)", n)
	}

	// Exactly one confirmation.expired (from the first lazy check, not the second).
	if n := countExpiredEventsFor(evs, callID); n != 1 {
		t.Errorf("AC-3: confirmation.expired count = %d, want 1", n)
	}
}

// TestExpiry_AbandonedMessage_StartTurnIsNoOp verifies that StartTurn on a conversation whose last assistant message
// is abandoned is a no-op (the turn completion guard treats abandoned as terminal).
func TestExpiry_AbandonedMessage_StartTurnIsNoOp(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	const toolName = "abandoned_guard_tool"
	extTool := capability.NewTool(
		toolName, "abandoned guard tool",
		json.RawMessage(`{"type":"object","properties":{"call_id":{"type":"string"}}}`),
		capability.RiskExternal,
		func(_ context.Context, _ store.Scope, _ struct {
			CallID string `json:"call_id"`
		}) (capability.Result, error) {
			return map[string]string{"result": "done"}, nil
		},
	)

	// Gateway will be checked if the guard fails.
	gw := &queuedChatter{rounds: [][]gateway.Chunk{{
		{TextDelta: "should not be called"},
		{Done: true},
	}}}
	h := buildHarnessWithClockAndTool(t, s, gw, bus, extTool, time.Now)
	_ = buildConfirmServer(h)

	// Insert conversation: user message + abandoned assistant message.
	convID := id.New()
	callID := id.New()
	scope := store.UserScope(p)
	sq := s.ForUser(scope)

	if err := sq.UpsertConversation(context.Background(), convID); err != nil {
		t.Fatalf("UpsertConversation: %v", err)
	}
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             id.New(),
		ConversationID: convID,
		Role:           "user",
		Content:        "test",
	}); err != nil {
		t.Fatalf("InsertMessage user: %v", err)
	}
	toolCallsJSON, _ := json.Marshal([]gateway.ToolCall{{
		ID:        callID,
		Name:      toolName,
		Arguments: json.RawMessage(`{"call_id":"test"}`),
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
	// Mark the assistant message abandoned directly.
	if err := sq.MarkMessageAbandoned(context.Background(), assistantMsgID); err != nil {
		t.Fatalf("MarkMessageAbandoned: %v", err)
	}

	// StartTurn on a conversation with an abandoned assistant message must be a no-op.
	origin := harness.Origin{Channel: "web", Trust: harness.Trusted}
	if err := h.StartTurn(context.Background(), convID, harness.InboundMessage{Content: "test"}, origin, p); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// No events emitted.
	evs := bus.snapshot()
	if n := countEventType(evs, "message.completed"); n != 0 {
		t.Errorf("AC-3 (StartTurn): message.completed emitted %d times, want 0 (abandoned → inert)", n)
	}
	if n := countEventType(evs, "turn.failed"); n != 0 {
		t.Errorf("AC-3 (StartTurn): turn.failed emitted %d times, want 0", n)
	}

	// Gateway must not have been called.
	if gw.pos != 0 {
		t.Errorf("AC-3 (StartTurn): gateway called %d times, want 0 (abandoned turn inert)", gw.pos)
	}
}

// ── AC-4: Concurrency — sweep vs Resolve on the same row ─────────────────────

// TestExpiry_ConcurrentSweepAndResolve verifies that when SweepExpiredConfirmations and Resolve race on the same
// lapsed pending row, exactly ONE wins the CAS and exactly ONE confirmation.expired event is emitted. No
// double-execution, no double event. Hammered under -race by the caller (-count=50 in the test runner).
func TestExpiry_ConcurrentSweepAndResolve(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	execTracker := newCountingTool()

	const toolName = "sweep_race_tool"
	extTool := capability.NewTool(
		toolName, "sweep race test tool",
		json.RawMessage(`{"type":"object","properties":{"call_id":{"type":"string"}}}`),
		capability.RiskExternal,
		func(_ context.Context, _ store.Scope, _ struct {
			CallID string `json:"call_id"`
		}) (capability.Result, error) {
			execTracker.record("executed")
			return map[string]string{"result": "done"}, nil
		},
	)

	// Clock: past, so Resolve sees it as expired and so does the sweep.
	pastTime := time.Now().UTC().Add(-2 * time.Hour)
	nowFn := func() time.Time { return pastTime }

	// Gateway should not be called (no model round on expiry).
	gw := &queuedChatter{rounds: [][]gateway.Chunk{{
		{TextDelta: "should not be called"},
		{Done: true},
	}}}
	h := buildHarnessWithClockAndTool(t, s, gw, bus, extTool, nowFn)
	srv := buildConfirmServer(h)

	pastExpiry := pastTime.Add(-time.Hour).Format(time.RFC3339)
	convID, callID, _ := insertSuspendedConversation(t, s, p, toolName, pastExpiry)

	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine 1: Resolve (approve — which will trigger lazy expiry).
	go func() {
		defer wg.Done()
		rr := resolveViaHTTP(t, srv, p, convID, callID, "approve")
		// Either 409 (expired by lazy check) or 409 (already resolved by sweep) — both are correct.
		if rr.Code != http.StatusConflict {
			t.Errorf("sweep race: Resolve status = %d, want 409", rr.Code)
		}
	}()

	// Goroutine 2: SweepExpiredConfirmations.
	go func() {
		defer wg.Done()
		n, err := h.SweepExpiredConfirmations(context.Background())
		if err != nil {
			t.Errorf("sweep race: SweepExpiredConfirmations: %v", err)
		}
		// Count is either 0 (Resolve won the CAS) or 1 (sweep won) — never > 1.
		if n > 1 {
			t.Errorf("sweep race: SweepExpiredConfirmations swept %d, want <= 1", n)
		}
	}()

	wg.Wait()

	// Allow any in-flight goroutines to settle.
	time.Sleep(200 * time.Millisecond)

	// Final row status must be "expired" (either winner set it).
	if st := getConfirmationStatus(t, s, callID, p.UserID); st != "expired" {
		t.Errorf("sweep race: final row status = %q, want 'expired'", st)
	}

	// Exactly ONE confirmation.expired event must have been emitted.
	evs := bus.snapshot()
	if n := countExpiredEventsFor(evs, callID); n != 1 {
		t.Errorf("sweep race: confirmation.expired count = %d, want 1 (exactly one winner)", n)
	}

	// Tool must NEVER have executed.
	if n := execTracker.countFor("executed"); n != 0 {
		t.Errorf("sweep race: tool executed %d times, want 0 (expiry never executes)", n)
	}

	// Gateway must NOT have been called.
	if gw.pos != 0 {
		t.Errorf("sweep race: gateway called %d times, want 0", gw.pos)
	}
}

// ── AC-5: Multi-confirm per-call expiry ──────────────────────────────────────

// insertSuspendedConversationMultiCall manually constructs a conversation suspended with N Confirm-tier tool calls on
// ONE assistant message. Returns the assistant message ID, conversation ID, and a slice of call IDs in insertion
// order. The expires_at for each call is set to the provided per-call expiresAt string.
func insertSuspendedConversationMultiCall(
	t *testing.T,
	s *store.Store,
	p store.Principal,
	toolName string,
	callCount int,
	expiresAts []string, // one per call; len must equal callCount
) (convID, assistantMsgID string, callIDs []string) {
	t.Helper()
	convID = id.New()
	scope := store.UserScope(p)
	sq := s.ForUser(scope)

	if err := sq.UpsertConversation(context.Background(), convID); err != nil {
		t.Fatalf("UpsertConversation: %v", err)
	}
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             id.New(),
		ConversationID: convID,
		Role:           "user",
		Content:        "please do many things",
	}); err != nil {
		t.Fatalf("InsertMessage user: %v", err)
	}

	callIDs = make([]string, callCount)
	calls := make([]gateway.ToolCall, callCount)
	for i := range callCount {
		callIDs[i] = id.New()
		calls[i] = gateway.ToolCall{
			ID:        callIDs[i],
			Name:      toolName,
			Arguments: json.RawMessage(`{"call_id":"test"}`),
		}
	}

	toolCallsJSON, _ := json.Marshal(calls)
	assistantMsgID = id.New()
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             assistantMsgID,
		ConversationID: convID,
		Role:           "assistant",
		Content:        "",
		ToolCalls:      string(toolCallsJSON),
	}); err != nil {
		t.Fatalf("InsertMessage assistant: %v", err)
	}

	for i, cid := range callIDs {
		if _, err := s.Writer().ExecContext(context.Background(),
			`INSERT INTO pending_confirmations
			 (id, conversation_id, message_id, user_id, tool_name, args, risk, status, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
			cid, convID, assistantMsgID, p.UserID, toolName,
			`{"call_id":"test"}`, "external", expiresAts[i],
		); err != nil {
			t.Fatalf("insert pending_confirmations[%d]: %v", i, err)
		}
	}
	return convID, assistantMsgID, callIDs
}

// hasToolResultForCall returns true if a tool-result message with the given callID exists in the conversation.
func hasToolResultForCall(t *testing.T, s *store.Store, userID, convID, callID string) bool {
	t.Helper()
	var n int
	if err := s.Reader().QueryRowContext(context.Background(),
		`SELECT count(*) FROM messages WHERE conversation_id = ? AND user_id = ? AND role = 'tool' AND tool_call_id = ?`,
		convID, userID, callID,
	).Scan(&n); err != nil {
		t.Fatalf("hasToolResultForCall: %v", err)
	}
	return n > 0
}

// TestExpiry_MultiConfirm_ApproveAfterOneSiblingExpires is the MUST-FAIL-BEFORE-FIX TDD test for the multi-confirmation
// per-call expiry bug.
//
// Setup: one assistant message with THREE Confirm-tier calls (callA, callB, callC), all pending. Expire ONE call
// (callA) via the lazy path (clock set to past). Then APPROVE a still-pending sibling (callB). Assert:
//   - callA gets a synthetic "expired" tool-result persisted.
//   - confirmation.expired fired for callA.
//   - callB's tool EXECUTES exactly once after approval.
//   - callB's tool-result is persisted.
//   - The assistant message is NOT abandoned (callC still pending).
//   - callC is still pending (not affected by callA's expiry).
//   - The turn does not reach message.completed (callC still pending).
//
// After fixing callC, the turn completes with message.completed. Without the fix: callA's expiry marks the whole
// message abandoned; approving callB spawns run, outstandingToolCalls returns nil (abandoned short-circuit),
// isTurnComplete returns true, and the turn silently dies with 202 to the user but no execution.
func TestExpiry_MultiConfirm_ApproveAfterOneSiblingExpires(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := newCountingTool()

	const toolName = "multi_confirm_expire_tool"
	extTool := capability.NewTool(
		toolName, "multi-confirm expiry test tool",
		json.RawMessage(`{"type":"object","properties":{"call_id":{"type":"string"}}}`),
		capability.RiskExternal,
		func(_ context.Context, _ store.Scope, _ struct {
			CallID string `json:"call_id"`
		}) (capability.Result, error) {
			tracker.record("executed")
			return map[string]string{"result": "done"}, nil
		},
	)

	// Clock: past so that callA's expiry triggers lazily.
	pastTime := time.Now().UTC().Add(-2 * time.Hour)
	pastExpiry := pastTime.Add(-time.Hour).Format(time.RFC3339)
	futureExpiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)

	// callA expires in the past, callB and callC expire in the future.
	expiresAts := []string{pastExpiry, futureExpiry, futureExpiry}

	// Gateway: after callB (and eventually callC) are approved and tool results
	// (including callA's synthetic expired result) are all in, the model gives a
	// final reply.
	roundFinal := []gateway.Chunk{{TextDelta: "all resolved"}, {Done: true}}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{roundFinal}}

	nowFn := func() time.Time { return pastTime }
	h := buildHarnessWithClockAndTool(t, s, gw, bus, extTool, nowFn)
	srv := buildConfirmServer(h)

	convID, assistantMsgID, callIDs := insertSuspendedConversationMultiCall(
		t, s, p, toolName, 3, expiresAts,
	)
	callA, callB, callC := callIDs[0], callIDs[1], callIDs[2]

	// Step 1: Attempt to approve callA — it should be rejected as expired (past ExpiresAt). This triggers the lazy
	// expiry path for callA.
	rrA := resolveViaHTTP(t, srv, p, convID, callA, "approve")
	if rrA.Code != http.StatusConflict {
		t.Fatalf("multi-confirm: approve callA status = %d, want 409 (expired); body: %s", rrA.Code, rrA.Body.String())
	}
	if !strings.Contains(rrA.Body.String(), "confirmation_expired") {
		t.Errorf("multi-confirm: response body missing 'confirmation_expired'; body: %s", rrA.Body.String())
	}

	// Allow expiry processing to settle.
	time.Sleep(100 * time.Millisecond)

	// Assert: callA row = "expired".
	if st := getConfirmationStatus(t, s, callA, p.UserID); st != "expired" {
		t.Errorf("multi-confirm: callA status = %q, want 'expired'", st)
	}

	// Assert: callA has a synthetic expired tool-result persisted.
	if !hasToolResultForCall(t, s, p.UserID, convID, callA) {
		t.Error("multi-confirm: callA must have a synthetic expired tool-result persisted (MISSING — bug)")
	}

	// Assert: confirmation.expired emitted for callA.
	if !hasExpiredEvent(bus.snapshot(), callA) {
		t.Error("multi-confirm: confirmation.expired not emitted for callA")
	}

	// Assert: assistant message is NOT abandoned (callB and callC are still pending).
	if isMessageAbandoned(t, s, assistantMsgID, p.UserID) {
		t.Error("multi-confirm: assistant message abandoned while callB/callC still pending (WRONG — bug)")
	}

	// Assert: callB and callC still pending.
	if st := getConfirmationStatus(t, s, callB, p.UserID); st != "pending" {
		t.Errorf("multi-confirm: callB status = %q, want 'pending'", st)
	}
	if st := getConfirmationStatus(t, s, callC, p.UserID); st != "pending" {
		t.Errorf("multi-confirm: callC status = %q, want 'pending'", st)
	}

	// Step 2: Approve callB (still pending sibling). This should resume the turn, execute callB's tool, and leave callC
	// still pending (turn re-suspends). Under the bug: the message is already abandoned → outstandingToolCalls returns
	// nil → isTurnComplete returns true → the turn silently dies with 202 but no execution.
	rrB := resolveViaHTTP(t, srv, p, convID, callB, "approve")
	if rrB.Code != http.StatusAccepted {
		t.Fatalf("multi-confirm: approve callB status = %d, want 202; body: %s", rrB.Code, rrB.Body.String())
	}

	// Wait for callB's tool to execute (or timeout if the bug causes a silent hang).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tracker.countFor("executed") >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Assert: callB's tool executed exactly once.
	if n := tracker.countFor("executed"); n != 1 {
		t.Errorf("multi-confirm: tool executed %d times after approving callB, want 1 (SILENT HANG BUG)", n)
	}

	// Assert: callB has a tool-result persisted.
	if !hasToolResultForCall(t, s, p.UserID, convID, callB) {
		t.Error("multi-confirm: callB tool-result not persisted after approval")
	}

	// Assert: turn not yet complete (callC still pending → re-suspended).
	time.Sleep(50 * time.Millisecond)
	evs := bus.snapshot()
	if n := countEventType(evs, "message.completed"); n != 0 {
		t.Errorf("multi-confirm: message.completed emitted %d times after approving callB only, want 0 (callC still pending)", n)
	}

	// Step 3: Approve callC — all calls now resolved. Turn should complete.
	rrC := resolveViaHTTP(t, srv, p, convID, callC, "approve")
	if rrC.Code != http.StatusAccepted {
		t.Fatalf("multi-confirm: approve callC status = %d, want 202; body: %s", rrC.Code, rrC.Body.String())
	}

	// Wait for the turn to complete with all 2 tool executions (callB + callC).
	waitForCompleted(t, bus, 2, 8*time.Second)

	// Assert: turn reached message.completed.
	evsFinal := bus.snapshot()
	if n := countEventType(evsFinal, "message.completed"); n != 1 {
		t.Errorf("multi-confirm: message.completed count = %d, want 1 after all calls resolved", n)
	}

	// Assert: callC executed.
	if n := tracker.countFor("executed"); n != 2 {
		t.Errorf("multi-confirm: total tool executions = %d, want 2 (callB + callC)", n)
	}

	// Assert: message is NOT abandoned (mixed case — not all expired).
	if isMessageAbandoned(t, s, assistantMsgID, p.UserID) {
		t.Error("multi-confirm: assistant message abandoned in mixed (approved+expired) case — must NOT be abandoned")
	}

	// Assert: exactly one confirmation.expired for callA.
	if n := countExpiredEventsFor(evsFinal, callA); n != 1 {
		t.Errorf("multi-confirm: confirmation.expired count for callA = %d, want 1", n)
	}
}

// ── MUST-FIX: expiry-last stall (mixed-turn, C expires after B approved) ────────

// TestExpiry_ExpiryLast_LazyPath is the TDD test for the mixed-turn stall bug: three Confirm-tier calls (callA, callB,
// callC) on ONE assistant message.
//
//  1. callA expires lazily (clock past ExpiresAt) — synthetic expired tool-result persisted, confirmation.expired
//     emitted, message NOT abandoned (B+C pending).
//  2. User approves callB — run executes B, suspends on callC still pending.
//  3. callC expires lazily (second Resolve call on expired callC).
//
// Before the fix: the expiry path in Resolve RETURNS without re-entering run, so callC has a synthetic tool-result,
// the message is NOT abandoned (B is approved), but NOTHING fires the continue round. The turn silently stalls — no
// message.completed, no turn.failed. The test must see EXACTLY ONE message.completed after step 3.
//
// After the fix: the non-abandoning expiry path re-enters run (mirror of the approve/deny resume goroutine), the
// continue round narrates B's executed result, and message.completed fires.
func TestExpiry_ExpiryLast_LazyPath(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := newCountingTool()

	const toolName = "expiry_last_lazy_tool"
	extTool := capability.NewTool(
		toolName, "expiry-last lazy path test tool",
		json.RawMessage(`{"type":"object","properties":{"call_id":{"type":"string"}}}`),
		capability.RiskExternal,
		func(_ context.Context, _ store.Scope, _ struct {
			CallID string `json:"call_id"`
		}) (capability.Result, error) {
			tracker.record("executed")
			return map[string]string{"result": "done"}, nil
		},
	)

	// Clock: past, so all calls with pastExpiry are lapsed.
	pastTime := time.Now().UTC().Add(-2 * time.Hour)
	pastExpiry := pastTime.Add(-time.Hour).Format(time.RFC3339)
	futureExpiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)

	// callA and callC expire; callB has a future expiry (still valid when approved).
	expiresAts := []string{pastExpiry, futureExpiry, pastExpiry}

	// After all three calls have tool-results (callA expired, callB executed, callC expired), the model gives a final
	// reply.
	roundFinal := []gateway.Chunk{{TextDelta: "all resolved"}, {Done: true}}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{roundFinal}}

	nowFn := func() time.Time { return pastTime }
	h := buildHarnessWithClockAndTool(t, s, gw, bus, extTool, nowFn)
	srv := buildConfirmServer(h)

	convID, assistantMsgID, callIDs := insertSuspendedConversationMultiCall(
		t, s, p, toolName, 3, expiresAts,
	)
	callA, callB, callC := callIDs[0], callIDs[1], callIDs[2]

	// Step 1: Attempt to approve callA — it should be rejected as expired (lazy).
	rrA := resolveViaHTTP(t, srv, p, convID, callA, "approve")
	if rrA.Code != http.StatusConflict {
		t.Fatalf("expiry-last lazy: approve callA status = %d, want 409; body: %s", rrA.Code, rrA.Body.String())
	}
	time.Sleep(100 * time.Millisecond)

	// callA must be expired with synthetic tool-result.
	if st := getConfirmationStatus(t, s, callA, p.UserID); st != "expired" {
		t.Errorf("expiry-last lazy: callA status = %q, want 'expired'", st)
	}
	if !hasToolResultForCall(t, s, p.UserID, convID, callA) {
		t.Error("expiry-last lazy: callA missing synthetic expired tool-result")
	}
	if !hasExpiredEvent(bus.snapshot(), callA) {
		t.Error("expiry-last lazy: confirmation.expired not emitted for callA")
	}

	// Message must NOT be abandoned (callB and callC still pending).
	if isMessageAbandoned(t, s, assistantMsgID, p.UserID) {
		t.Fatal("expiry-last lazy: message abandoned while callB/callC still pending — wrong")
	}

	// Step 2: Approve callB (still valid). This should resume the turn, execute callB's tool, and re-suspend on callC
	// still pending.
	rrB := resolveViaHTTP(t, srv, p, convID, callB, "approve")
	if rrB.Code != http.StatusAccepted {
		t.Fatalf("expiry-last lazy: approve callB status = %d, want 202; body: %s", rrB.Code, rrB.Body.String())
	}

	// Wait for callB's tool to execute.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tracker.countFor("executed") >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := tracker.countFor("executed"); n != 1 {
		t.Errorf("expiry-last lazy: tool executed %d times after approving callB, want 1", n)
	}

	// Turn must NOT have completed yet (callC still pending).
	time.Sleep(50 * time.Millisecond)
	if n := countEventType(bus.snapshot(), "message.completed"); n != 0 {
		t.Errorf("expiry-last lazy: message.completed emitted %d times after callB approved only — callC still pending", n)
	}

	// Step 3: Expire callC via the lazy path (Resolve on expired callC). BEFORE THE FIX: this returns without
	// re-entering run → silent stall. AFTER THE FIX: re-enters run → continue round fires → message.completed.
	rrC := resolveViaHTTP(t, srv, p, convID, callC, "approve")
	if rrC.Code != http.StatusConflict {
		t.Fatalf("expiry-last lazy: approve callC status = %d, want 409 (expired); body: %s", rrC.Code, rrC.Body.String())
	}

	// callC must now be expired with synthetic tool-result.
	time.Sleep(50 * time.Millisecond)
	if st := getConfirmationStatus(t, s, callC, p.UserID); st != "expired" {
		t.Errorf("expiry-last lazy: callC status = %q, want 'expired'", st)
	}
	if !hasToolResultForCall(t, s, p.UserID, convID, callC) {
		t.Error("expiry-last lazy: callC missing synthetic expired tool-result")
	}
	if !hasExpiredEvent(bus.snapshot(), callC) {
		t.Error("expiry-last lazy: confirmation.expired not emitted for callC")
	}

	// Message must NOT be abandoned (mixed case: B approved, A+C expired).
	if isMessageAbandoned(t, s, assistantMsgID, p.UserID) {
		t.Error("expiry-last lazy: message abandoned in mixed (approved+expired) case — must NOT be abandoned")
	}

	// Now wait for the turn to complete — this is the MUST-FIX assertion. Without the fix, the turn stalls here and
	// message.completed never fires.
	waitForCompleted(t, bus, 1, 8*time.Second)

	evsFinal := bus.snapshot()
	if n := countEventType(evsFinal, "message.completed"); n != 1 {
		t.Errorf("expiry-last lazy: message.completed count = %d, want 1 (STALL BUG: expiry-last does not re-enter run)", n)
	}

	// callB executed exactly once.
	if n := tracker.countFor("executed"); n != 1 {
		t.Errorf("expiry-last lazy: tool executed %d times total, want 1 (callB only)", n)
	}

	// Exactly one confirmation.expired per expired call.
	if n := countExpiredEventsFor(evsFinal, callA); n != 1 {
		t.Errorf("expiry-last lazy: confirmation.expired count for callA = %d, want 1", n)
	}
	if n := countExpiredEventsFor(evsFinal, callC); n != 1 {
		t.Errorf("expiry-last lazy: confirmation.expired count for callC = %d, want 1", n)
	}
}

// TestExpiry_ExpiryLast_SweepPath mirrors TestExpiry_ExpiryLast_LazyPath but uses SweepExpiredConfirmations to expire
// callC (instead of the lazy Resolve path). The same stall manifests via the sweep: callC's expiry does not re-enter
// run, so the continue round never fires.
func TestExpiry_ExpiryLast_SweepPath(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := newCountingTool()

	const toolName = "expiry_last_sweep_tool"
	extTool := capability.NewTool(
		toolName, "expiry-last sweep path test tool",
		json.RawMessage(`{"type":"object","properties":{"call_id":{"type":"string"}}}`),
		capability.RiskExternal,
		func(_ context.Context, _ store.Scope, _ struct {
			CallID string `json:"call_id"`
		}) (capability.Result, error) {
			tracker.record("executed")
			return map[string]string{"result": "done"}, nil
		},
	)

	// Stage 1 clock: callA expires, callB and callC are future.
	stage1Time := time.Now().UTC().Add(-2 * time.Hour)
	pastExpiry := stage1Time.Add(-time.Hour).Format(time.RFC3339)
	// callB has a future expiry so it can be approved at stage1 time.
	futureExpiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)

	// callA expires; callB is valid (approved later); callC expires. For the sweep test: callC will also have
	// pastExpiry, but we drive expiry via the sweep AFTER callB is approved.
	expiresAts := []string{pastExpiry, futureExpiry, pastExpiry}

	// After all three calls have tool-results, the model gives a final reply.
	roundFinal := []gateway.Chunk{{TextDelta: "sweep resolved"}, {Done: true}}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{roundFinal}}

	// Clock returns stage1Time (past), making all pastExpiry rows lapsed.
	nowFn := func() time.Time { return stage1Time }
	h := buildHarnessWithClockAndTool(t, s, gw, bus, extTool, nowFn)
	srv := buildConfirmServer(h)

	convID, assistantMsgID, callIDs := insertSuspendedConversationMultiCall(
		t, s, p, toolName, 3, expiresAts,
	)
	callA, callB, callC := callIDs[0], callIDs[1], callIDs[2]

	// Step 1: Expire callA via the lazy path.
	rrA := resolveViaHTTP(t, srv, p, convID, callA, "approve")
	if rrA.Code != http.StatusConflict {
		t.Fatalf("expiry-last sweep: expire callA status = %d, want 409", rrA.Code)
	}
	time.Sleep(100 * time.Millisecond)

	if st := getConfirmationStatus(t, s, callA, p.UserID); st != "expired" {
		t.Errorf("expiry-last sweep: callA status = %q, want 'expired'", st)
	}
	if !hasToolResultForCall(t, s, p.UserID, convID, callA) {
		t.Error("expiry-last sweep: callA missing synthetic expired tool-result")
	}
	if isMessageAbandoned(t, s, assistantMsgID, p.UserID) {
		t.Fatal("expiry-last sweep: message abandoned while callB/callC still pending — wrong")
	}

	// Step 2: Approve callB.
	rrB := resolveViaHTTP(t, srv, p, convID, callB, "approve")
	if rrB.Code != http.StatusAccepted {
		t.Fatalf("expiry-last sweep: approve callB status = %d, want 202", rrB.Code)
	}

	// Wait for callB's tool to execute.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tracker.countFor("executed") >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := tracker.countFor("executed"); n != 1 {
		t.Errorf("expiry-last sweep: tool executed %d times after approving callB, want 1", n)
	}

	// Turn still pending (callC outstanding).
	time.Sleep(50 * time.Millisecond)
	if n := countEventType(bus.snapshot(), "message.completed"); n != 0 {
		t.Errorf("expiry-last sweep: message.completed emitted %d times after callB only", n)
	}

	// Step 3: Expire callC via the sweep. BEFORE THE FIX: sweep processes callC, marks it expired, but does NOT
	// re-enter run for the affected conversation → continue round never fires. AFTER THE FIX: sweep collects the
	// non-abandoned conv IDs and re-enters run once per conversation after processing.
	n, sweepErr := h.SweepExpiredConfirmations(context.Background())
	if sweepErr != nil {
		t.Fatalf("expiry-last sweep: SweepExpiredConfirmations: %v", sweepErr)
	}
	// Sweep should have found exactly callC (callA was already expired before this sweep).
	if n != 1 {
		t.Errorf("expiry-last sweep: swept count = %d, want 1 (callC only)", n)
	}

	// callC must be expired.
	time.Sleep(50 * time.Millisecond)
	if st := getConfirmationStatus(t, s, callC, p.UserID); st != "expired" {
		t.Errorf("expiry-last sweep: callC status = %q, want 'expired'", st)
	}
	if !hasToolResultForCall(t, s, p.UserID, convID, callC) {
		t.Error("expiry-last sweep: callC missing synthetic expired tool-result")
	}
	if !hasExpiredEvent(bus.snapshot(), callC) {
		t.Error("expiry-last sweep: confirmation.expired not emitted for callC")
	}

	// Message must NOT be abandoned (mixed case: B approved, A+C expired).
	if isMessageAbandoned(t, s, assistantMsgID, p.UserID) {
		t.Error("expiry-last sweep: message abandoned in mixed case — must NOT be abandoned")
	}

	// The turn must now complete — this is the MUST-FIX assertion for the sweep path. Without the fix, the sweep
	// expires callC but never triggers the continue round.
	waitForCompleted(t, bus, 1, 8*time.Second)

	evsFinal := bus.snapshot()
	if n := countEventType(evsFinal, "message.completed"); n != 1 {
		t.Errorf("expiry-last sweep: message.completed count = %d, want 1 (STALL BUG: sweep does not re-enter run)", n)
	}

	// callB executed exactly once.
	if n := tracker.countFor("executed"); n != 1 {
		t.Errorf("expiry-last sweep: tool executed %d times total, want 1 (callB only)", n)
	}
	if n := countExpiredEventsFor(evsFinal, callA); n != 1 {
		t.Errorf("expiry-last sweep: confirmation.expired count for callA = %d, want 1", n)
	}
	if n := countExpiredEventsFor(evsFinal, callC); n != 1 {
		t.Errorf("expiry-last sweep: confirmation.expired count for callC = %d, want 1", n)
	}
}

// TestExpiry_MultiConfirm_AllExpire_MessageAbandoned verifies the fully-stale case: all confirm rows for a message
// expire (none approved/denied). The message MUST be abandoned, NO model round fires, and the turn is inert.
func TestExpiry_MultiConfirm_AllExpire_MessageAbandoned(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := newCountingTool()

	const toolName = "all_expire_tool"
	extTool := capability.NewTool(
		toolName, "all-expire test tool",
		json.RawMessage(`{"type":"object","properties":{"call_id":{"type":"string"}}}`),
		capability.RiskExternal,
		func(_ context.Context, _ store.Scope, _ struct {
			CallID string `json:"call_id"`
		}) (capability.Result, error) {
			tracker.record("executed")
			return map[string]string{"result": "done"}, nil
		},
	)

	// All calls expired.
	pastTime := time.Now().UTC().Add(-2 * time.Hour)
	pastExpiry := pastTime.Add(-time.Hour).Format(time.RFC3339)
	expiresAts := []string{pastExpiry, pastExpiry}

	gw := &queuedChatter{rounds: [][]gateway.Chunk{{{TextDelta: "should not call"}, {Done: true}}}}
	nowFn := func() time.Time { return pastTime }
	h := buildHarnessWithClockAndTool(t, s, gw, bus, extTool, nowFn)
	srv := buildConfirmServer(h)

	convID, assistantMsgID, callIDs := insertSuspendedConversationMultiCall(
		t, s, p, toolName, 2, expiresAts,
	)
	callA, callB := callIDs[0], callIDs[1]

	// Expire callA via lazy path.
	rrA := resolveViaHTTP(t, srv, p, convID, callA, "approve")
	if rrA.Code != http.StatusConflict {
		t.Fatalf("all-expire: expire callA status = %d, want 409", rrA.Code)
	}
	time.Sleep(100 * time.Millisecond)

	// callA expired, callB still pending → message must NOT be abandoned yet.
	if isMessageAbandoned(t, s, assistantMsgID, p.UserID) {
		t.Error("all-expire: message abandoned after only callA expired — callB still pending (WRONG)")
	}

	// Expire callB via lazy path.
	rrB := resolveViaHTTP(t, srv, p, convID, callB, "approve")
	if rrB.Code != http.StatusConflict {
		t.Fatalf("all-expire: expire callB status = %d, want 409", rrB.Code)
	}
	time.Sleep(100 * time.Millisecond)

	// Both expired → message must now be abandoned.
	if !isMessageAbandoned(t, s, assistantMsgID, p.UserID) {
		t.Error("all-expire: message NOT abandoned after ALL calls expired — must be abandoned")
	}

	// No model round should fire.
	evs := bus.snapshot()
	if n := countEventType(evs, "message.completed"); n != 0 {
		t.Errorf("all-expire: message.completed emitted %d times — no model round on full expiry", n)
	}
	if gw.pos != 0 {
		t.Errorf("all-expire: gateway called %d times, want 0", gw.pos)
	}

	// Tool never executed.
	if n := tracker.countFor("executed"); n != 0 {
		t.Errorf("all-expire: tool executed %d times, want 0", n)
	}

	// Both calls have synthetic expired tool-results.
	if !hasToolResultForCall(t, s, p.UserID, convID, callA) {
		t.Error("all-expire: callA missing synthetic expired tool-result")
	}
	if !hasToolResultForCall(t, s, p.UserID, convID, callB) {
		t.Error("all-expire: callB missing synthetic expired tool-result")
	}

	// Two expired events emitted.
	if !hasExpiredEvent(evs, callA) {
		t.Error("all-expire: confirmation.expired not emitted for callA")
	}
	if !hasExpiredEvent(evs, callB) {
		t.Error("all-expire: confirmation.expired not emitted for callB")
	}
}

// ── NICE-TO-HAVE: shape of synthetic expired tool-result ─────────────────────

// TestExpiry_SyntheticResultShape asserts that the synthetic expired tool-result persisted for an expired call has the
// model-safe JSON shape:
//
//	{"error":"This confirmation expired; the action was not performed.","code":"confirmation_expired"}
//
// This verifies the exact content the model receives in the continue round, not just that a row was persisted. A wrong
// payload (e.g. a plain string or a different error key) would cause the model to misinterpret the expiry.
func TestExpiry_SyntheticResultShape(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	const toolName = "shape_ext_tool"
	extTool := capability.NewTool(
		toolName, "shape test tool",
		json.RawMessage(`{"type":"object","properties":{"call_id":{"type":"string"}}}`),
		capability.RiskExternal,
		func(_ context.Context, _ store.Scope, _ struct {
			CallID string `json:"call_id"`
		}) (capability.Result, error) {
			return map[string]string{"result": "done"}, nil
		},
	)

	pastTime := time.Now().UTC().Add(-2 * time.Hour)
	nowFn := func() time.Time { return pastTime }
	gw := &queuedChatter{rounds: [][]gateway.Chunk{{{Done: true}}}}
	h := buildHarnessWithClockAndTool(t, s, gw, bus, extTool, nowFn)
	srv := buildConfirmServer(h)

	pastExpiry := pastTime.Add(-time.Hour).Format(time.RFC3339)
	convID, callID, _ := insertSuspendedConversation(t, s, p, toolName, pastExpiry)

	// Trigger expiry via the lazy path.
	rr := resolveViaHTTP(t, srv, p, convID, callID, "approve")
	if rr.Code != http.StatusConflict {
		t.Fatalf("shape: expire status = %d, want 409; body: %s", rr.Code, rr.Body.String())
	}
	time.Sleep(100 * time.Millisecond)

	// Load the raw content of the synthetic tool-result message.
	var rawContent string
	err := s.Reader().QueryRowContext(context.Background(),
		`SELECT content FROM messages WHERE conversation_id = ? AND user_id = ? AND role = 'tool' AND tool_call_id = ?`,
		convID, p.UserID, callID,
	).Scan(&rawContent)
	if err != nil {
		t.Fatalf("shape: query synthetic tool-result: %v", err)
	}
	if rawContent == "" {
		t.Fatal("shape: synthetic tool-result content is empty")
	}

	// Unmarshal and verify the exact shape.
	var payload map[string]string
	if err := json.Unmarshal([]byte(rawContent), &payload); err != nil {
		t.Fatalf("shape: unmarshal synthetic tool-result %q: %v", rawContent, err)
	}

	const wantError = "This confirmation expired; the action was not performed."
	const wantCode = "confirmation_expired"

	if got := payload["error"]; got != wantError {
		t.Errorf("shape: payload[\"error\"] = %q, want %q", got, wantError)
	}
	if got := payload["code"]; got != wantCode {
		t.Errorf("shape: payload[\"code\"] = %q, want %q", got, wantCode)
	}

	// No extra keys — the model should see a clean two-key object.
	if len(payload) != 2 {
		t.Errorf("shape: payload has %d keys, want exactly 2; content: %s", len(payload), rawContent)
	}
}
