package harness_test

// policy_integration_test.go — black-box integration tests for trust-gating behavior.
// These tests verify the observable outcomes (events, persisted data, assembled catalog)
// for the gate-tool-execution-by-risk-and-trust feature.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/capability"
	"github.com/qovira/qovira/internal/gateway"
	"github.com/qovira/qovira/internal/harness"
	"github.com/qovira/qovira/internal/id"
	"github.com/qovira/qovira/internal/store"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// makeRiskTool builds a capability.Tool at the given RiskTier that records calls.
func makeRiskTool(name string, risk capability.RiskTier, tracker *callCapture) capability.Tool {
	type args struct {
		CallID string `json:"call_id"`
	}
	return capability.NewTool(
		name, "risk-tiered test tool",
		json.RawMessage(`{"type":"object","properties":{"call_id":{"type":"string"}}}`),
		risk,
		func(_ context.Context, _ store.Scope, a args) (capability.Result, error) {
			tracker.record(a.CallID)
			return map[string]string{"echo": a.CallID}, nil
		},
	)
}

// startTurnWithOriginAndWait fires a turn with a specific origin and waits for
// the terminal event. It returns the full event snapshot; the caller can also
// check whether run suspended by using waitForSuspendOrCompleted.
func startTurnWithOriginAndWait(
	t *testing.T,
	h *harness.Harness,
	s *store.Store,
	p store.Principal,
	bus *fakeBus,
	convID string,
	origin harness.Origin,
	minToolStarted int,
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

	if err := h.StartTurn(context.Background(), convID, harness.InboundMessage{Content: "test message"}, origin, p); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	waitForCompleted(t, bus, minToolStarted, 5*time.Second)
	return bus.snapshot()
}

// waitForSuspendOrCompleted waits for either a terminal event (message.completed or
// turn.failed) or for the given duration to pass without one, then returns the snapshot.
// Used to detect that a turn suspended (no terminal event emitted within the window).
func waitForSuspendOrCompleted(t *testing.T, bus *fakeBus, timeout time.Duration) []fakeEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		evs := bus.snapshot()
		for _, e := range evs {
			if e.event.Type == "message.completed" || e.event.Type == "turn.failed" {
				return evs
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	return bus.snapshot()
}

// hasTerminalEvent returns true if the snapshot contains a message.completed or turn.failed event.
func hasTerminalEvent(evs []fakeEvent) bool {
	for _, e := range evs {
		if e.event.Type == "message.completed" || e.event.Type == "turn.failed" {
			return true
		}
	}
	return false
}

// hasToolResult returns true if a tool-result message with the given callID
// appears in the persisted messages for this conversation and user.
func hasToolResult(t *testing.T, s *store.Store, p store.Principal, convID, callID string) bool {
	t.Helper()
	scope := store.UserScope(p)
	sq := s.ForUser(scope)
	msgs, err := sq.ListMessages(context.Background(), convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID.Valid && m.ToolCallID.String == callID {
			return true
		}
	}
	return false
}

// ── AC-2 (Trusted turn): Read and Write auto-execute; External and Destructive confirm/suspend ──

// TestPolicy_TrustedTurn_ReadWriteAutoExecute verifies that in a Trusted turn,
// RiskRead and RiskWrite tools are auto-executed (results persisted, tool.completed emitted).
func TestPolicy_TrustedTurn_ReadWriteAutoExecute(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := &callCapture{}

	reg := capability.NewRegistry()
	readTool := makeRiskTool("read_tool", capability.RiskRead, tracker)
	writeTool := makeRiskTool("write_tool", capability.RiskWrite, tracker)
	if err := reg.Add(fakeSource{tools: []capability.Tool{readTool, writeTool}}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	const (
		callIDRead  = "call_read"
		callIDWrite = "call_write"
	)
	round1 := []gateway.Chunk{
		toolCallChunk(callIDRead, "read_tool", `{"call_id":"read_val"}`),
		toolCallChunk(callIDWrite, "write_tool", `{"call_id":"write_val"}`),
		{Done: true},
	}
	round2 := []gateway.Chunk{{TextDelta: "done"}, {Done: true}}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1, round2}}
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	origin := harness.Origin{Channel: "web", Trust: harness.Trusted}
	_ = startTurnWithOriginAndWait(t, h, s, p, bus, convID, origin, 2)

	// Both tools must have been executed.
	called := tracker.snapshot()
	if len(called) != 2 {
		t.Errorf("called count = %d, want 2 (both Read and Write must auto-execute)", len(called))
	}

	// Tool results must be persisted.
	if !hasToolResult(t, s, p, convID, callIDRead) {
		t.Error("RiskRead tool result not persisted — should auto-execute")
	}
	if !hasToolResult(t, s, p, convID, callIDWrite) {
		t.Error("RiskWrite tool result not persisted — should auto-execute")
	}

	// tool.completed events emitted for both.
	evs := bus.snapshot()
	var completedCallIDs []string
	for _, e := range evs {
		if e.event.Type == "tool.completed" {
			if p2, ok := e.event.Data.(harness.ToolCompletedPayload); ok {
				completedCallIDs = append(completedCallIDs, p2.CallID)
			}
		}
	}
	if len(completedCallIDs) != 2 {
		t.Errorf("tool.completed count = %d, want 2", len(completedCallIDs))
	}
}

// TestPolicy_TrustedTurn_ExternalConfirmSuspends verifies that in a Trusted turn,
// a RiskExternal tool call routes to the confirm seam and suspends the turn
// (no terminal event, no tool result persisted).
func TestPolicy_TrustedTurn_ExternalConfirmSuspends(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := &callCapture{}

	reg := capability.NewRegistry()
	extTool := makeRiskTool("ext_tool", capability.RiskExternal, tracker)
	if err := reg.Add(fakeSource{tools: []capability.Tool{extTool}}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	const callIDExt = "call_ext"
	round1 := []gateway.Chunk{
		toolCallChunk(callIDExt, "ext_tool", `{"call_id":"ext_val"}`),
		{Done: true},
	}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1}}
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	origin := harness.Origin{Channel: "web", Trust: harness.Trusted}

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
	if err := h.StartTurn(context.Background(), convID, harness.InboundMessage{Content: "test message"}, origin, p); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}

	// Gate the "no terminal yet" assertion on the positive signal that the turn
	// actually reached the Confirm-path: wait for confirmation.required (emitted by
	// insertPendingConfirmation when the turn suspends). This replaces a bare 500ms
	// sleep — if confirmation.required fires, the turn is past the policy gate and is
	// suspended; asserting no terminal event is then load-bearing, not vacuous.
	evs := waitForConfirmationRequired(t, bus, 3*time.Second)

	// The turn MUST suspend: no terminal event emitted.
	if hasTerminalEvent(evs) {
		t.Error("External tool in Trusted turn emitted a terminal event — should suspend without terminal")
	}

	// The tool must NOT have been executed.
	if len(tracker.snapshot()) != 0 {
		t.Error("RiskExternal tool was executed — should route to confirm seam and suspend")
	}

	// No tool-result persisted.
	if hasToolResult(t, s, p, convID, callIDExt) {
		t.Error("tool result persisted for RiskExternal — should not be (confirm seam does not execute)")
	}

	// tool.started must NOT be emitted for a Confirm-path call that suspends.
	// Emitting it without a resolving tool.completed/tool.failed would leave a
	// permanently spinning UI chip; the confirmation-suspend-resume slice will
	// emit confirmation.required instead.
	for _, e := range evs {
		if e.event.Type == "tool.started" {
			if pl, ok := e.event.Data.(harness.ToolStartedPayload); ok && pl.CallID == callIDExt {
				t.Error("tool.started emitted for Confirm-path RiskExternal tool — must not fire before the policy decision suspends the turn")
			}
		}
	}
}

// TestPolicy_TrustedTurn_DestructiveConfirmSuspends verifies that in a Trusted turn,
// a RiskDestructive tool call routes to the confirm seam and suspends the turn.
func TestPolicy_TrustedTurn_DestructiveConfirmSuspends(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := &callCapture{}

	reg := capability.NewRegistry()
	destTool := makeRiskTool("dest_tool", capability.RiskDestructive, tracker)
	if err := reg.Add(fakeSource{tools: []capability.Tool{destTool}}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	const callIDDest = "call_dest"
	round1 := []gateway.Chunk{
		toolCallChunk(callIDDest, "dest_tool", `{"call_id":"dest_val"}`),
		{Done: true},
	}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1}}
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	origin := harness.Origin{Channel: "web", Trust: harness.Trusted}

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
	if err := h.StartTurn(context.Background(), convID, harness.InboundMessage{Content: "test message"}, origin, p); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}

	// Gate on confirmation.required (positive signal that the turn reached the Confirm
	// path and is now suspended), replacing the bare 500ms sleep.
	evs := waitForConfirmationRequired(t, bus, 3*time.Second)

	// The turn MUST suspend: no terminal event.
	if hasTerminalEvent(evs) {
		t.Error("Destructive tool in Trusted turn emitted a terminal event — should suspend without terminal")
	}

	// Tool must NOT have been executed.
	if len(tracker.snapshot()) != 0 {
		t.Error("RiskDestructive tool was executed — should route to confirm seam and suspend")
	}

	// No tool-result persisted.
	if hasToolResult(t, s, p, convID, callIDDest) {
		t.Error("tool result persisted for RiskDestructive — should not be (confirm seam does not execute)")
	}
}

// ── AC-3 & AC-4: Block enforcement for Untrusted origin ──────────────────────

// TestPolicy_UntrustedTurn_CatalogOmitsDestructive verifies that for an Untrusted
// origin the assembled ChatRequest does NOT include Destructive tools in its tool list.
// Uses a captureChatter (from context_retry_test.go) to inspect the request.
func TestPolicy_UntrustedTurn_CatalogOmitsDestructive(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	readTool := capability.Tool{Name: "read_tool", Risk: capability.RiskRead}
	writeTool := capability.Tool{Name: "write_tool", Risk: capability.RiskWrite}
	extTool := capability.Tool{Name: "ext_tool", Risk: capability.RiskExternal}
	destTool := capability.Tool{Name: "dest_tool", Risk: capability.RiskDestructive}

	reg := capability.NewRegistry()
	if err := reg.Add(fakeSource{tools: []capability.Tool{readTool, writeTool, extTool, destTool}}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	gw := &captureChatter{chunks: []gateway.Chunk{{TextDelta: "ok"}, {Done: true}}}

	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	// Construct an Untrusted origin directly — the resolver never returns this in v0.1
	// but we can drive it directly in tests (Acceptance Criterion 4).
	origin := harness.Origin{Channel: "test", Trust: harness.Untrusted}

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
		t.Fatalf("InsertMessage: %v", err)
	}

	if err := h.StartTurn(context.Background(), convID, harness.InboundMessage{Content: "test"}, origin, p); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	waitForCompleted(t, bus, 0, 5*time.Second)

	req := gw.lastRequest()
	if len(req.Tools) == 0 && len(reg.Catalog()) > 0 {
		// If all non-destructive tools were also removed, that's a bug.
		t.Fatal("assembled request has no tools at all — only destructive should be omitted")
	}

	// dest_tool must be absent from the assembled request tools.
	for _, ts := range req.Tools {
		if ts.Name == "dest_tool" {
			t.Errorf("dest_tool (Destructive) found in catalog for Untrusted origin — should be filtered out")
		}
	}

	// Read, write, and external should be present.
	toolNames := make(map[string]bool, len(req.Tools))
	for _, ts := range req.Tools {
		toolNames[ts.Name] = true
	}
	if !toolNames["read_tool"] {
		t.Error("read_tool missing from Untrusted catalog — should be present")
	}
	if !toolNames["write_tool"] {
		t.Error("write_tool missing from Untrusted catalog — should be present")
	}
	if !toolNames["ext_tool"] {
		t.Error("ext_tool missing from Untrusted catalog — should be present")
	}
}

// TestPolicy_UntrustedTurn_BlockRefusesDestructiveTool verifies that for an Untrusted
// origin, calling a Destructive tool persists a model-visible refusal as the tool-result
// and emits tool.failed (the execution gate enforces Block).
func TestPolicy_UntrustedTurn_BlockRefusesDestructiveTool(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := &callCapture{}

	reg := capability.NewRegistry()
	destTool := makeRiskTool("dest_tool", capability.RiskDestructive, tracker)
	if err := reg.Add(fakeSource{tools: []capability.Tool{destTool}}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	const callIDDest = "call_block"
	// The model attempts to call the destructive tool despite the catalog filter
	// (the execution gate is the real boundary).
	round1 := []gateway.Chunk{
		toolCallChunk(callIDDest, "dest_tool", `{"call_id":"blocked"}`),
		{Done: true},
	}
	round2 := []gateway.Chunk{{TextDelta: "understood, cannot do that"}, {Done: true}}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1, round2}}

	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	// Untrusted origin — Destructive → Block.
	origin := harness.Origin{Channel: "test", Trust: harness.Untrusted}

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

	if err := h.StartTurn(context.Background(), convID, harness.InboundMessage{Content: "test message"}, origin, p); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	// minToolStarted=0: Block path does not emit tool.started (refusal, not attempt).
	waitForCompleted(t, bus, 0, 5*time.Second)
	evs := bus.snapshot()

	// The tool must NOT have been executed.
	if len(tracker.snapshot()) != 0 {
		t.Error("Destructive tool was executed for Untrusted origin — must be blocked")
	}

	// A tool-result refusal must be persisted (so the model sees it and the loop continues).
	if !hasToolResult(t, s, p, convID, callIDDest) {
		t.Error("no tool-result message persisted for Blocked call — model must see the refusal")
	}

	// The loop must continue (message.completed must be emitted after the refusal).
	var completedCount int
	for _, e := range evs {
		if e.event.Type == "message.completed" {
			completedCount++
		}
	}
	if completedCount != 1 {
		t.Errorf("message.completed count = %d, want 1 (loop must continue after Block refusal)", completedCount)
	}

	// tool.failed must be emitted (consistent with how other model-visible refusals surface).
	var toolFailedEvts []harness.ToolFailedPayload
	for _, e := range evs {
		if e.event.Type == "tool.failed" {
			if pl, ok := e.event.Data.(harness.ToolFailedPayload); ok {
				toolFailedEvts = append(toolFailedEvts, pl)
			}
		}
	}
	if len(toolFailedEvts) == 0 {
		t.Error("no tool.failed event for blocked Destructive call")
	}
	if len(toolFailedEvts) > 0 && toolFailedEvts[0].CallID != callIDDest {
		t.Errorf("tool.failed callId = %q, want %q", toolFailedEvts[0].CallID, callIDDest)
	}

	// tool.started must NOT be emitted for a Block-path call — Block is a refusal,
	// not an execution attempt. The UI sees only tool.failed with the refusal message.
	for _, e := range evs {
		if e.event.Type == "tool.started" {
			if pl, ok := e.event.Data.(harness.ToolStartedPayload); ok && pl.CallID == callIDDest {
				t.Error("tool.started emitted for Blocked Destructive call — must not fire on Block path")
			}
		}
	}
}

// ── AC-4: Untrusted column tested directly (resolver doesn't return it in v0.1) ──

// TestPolicy_UntrustedOrigin_WriteConfirmsViaBlock verifies that Write from Untrusted
// resolves to Confirm (not Block). Since Confirm currently suspends (seam for next slice),
// a Write call from Untrusted should suspend the turn.
func TestPolicy_UntrustedOrigin_WriteConfirms(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	tracker := &callCapture{}

	reg := capability.NewRegistry()
	writeTool := makeRiskTool("write_tool", capability.RiskWrite, tracker)
	if err := reg.Add(fakeSource{tools: []capability.Tool{writeTool}}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	const callIDWrite = "call_write_untrusted"
	round1 := []gateway.Chunk{
		toolCallChunk(callIDWrite, "write_tool", `{"call_id":"w_untrusted"}`),
		{Done: true},
	}
	gw := &queuedChatter{rounds: [][]gateway.Chunk{round1}}
	h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

	convID := id.New()
	// Direct Untrusted origin — exercises the Confirm column of the policy table.
	origin := harness.Origin{Channel: "test", Trust: harness.Untrusted}

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
	if err := h.StartTurn(context.Background(), convID, harness.InboundMessage{Content: "test message"}, origin, p); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}

	// Write/Untrusted → Confirm → suspend. Gate the negative assertion on the
	// positive signal that the turn actually reached the Confirm path: wait for
	// confirmation.required (emitted by insertPendingConfirmation when the turn
	// suspends). This replaces a bare 500ms sleep.
	evs := waitForConfirmationRequired(t, bus, 3*time.Second)
	if hasTerminalEvent(evs) {
		t.Error("Write/Untrusted turn emitted a terminal event — should suspend (Confirm routes to seam)")
	}
	if len(tracker.snapshot()) != 0 {
		t.Error("Write tool executed for Untrusted origin — Confirm should not execute")
	}
}

// TestPolicy_RaceClean_TrustGating runs the trust-gating flow under the race detector.
func TestPolicy_RaceClean_TrustGating(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		trust  harness.TrustLevel
		risk   capability.RiskTier
		wantEx bool // whether the tool should be executed
	}{
		{name: "Trusted/Read", trust: harness.Trusted, risk: capability.RiskRead, wantEx: true},
		{name: "Trusted/Write", trust: harness.Trusted, risk: capability.RiskWrite, wantEx: true},
		// Trusted/External and Trusted/Destructive suspend — no terminal event.
		// Untrusted/Destructive → Block — loop continues.
		{name: "Untrusted/Read", trust: harness.Untrusted, risk: capability.RiskRead, wantEx: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := openMigratedStore(t)
			p := makeTestUser(t, s)
			bus := &fakeBus{}
			tracker := &callCapture{}

			reg := capability.NewRegistry()
			tool := makeRiskTool("test_tool", tt.risk, tracker)
			if err := reg.Add(fakeSource{tools: []capability.Tool{tool}}); err != nil {
				t.Fatalf("registry.Add: %v", err)
			}

			const callID = "call_race"
			round1 := []gateway.Chunk{
				toolCallChunk(callID, "test_tool", `{"call_id":"race_val"}`),
				{Done: true},
			}
			round2 := []gateway.Chunk{{TextDelta: "done"}, {Done: true}}
			gw := &queuedChatter{rounds: [][]gateway.Chunk{round1, round2}}
			h := buildHarnessWithTools(t, s, gw, bus, reg, harness.Config{})

			convID := id.New()
			origin := harness.Origin{Channel: "test", Trust: tt.trust}

			if tt.wantEx {
				_ = startTurnWithOriginAndWait(t, h, s, p, bus, convID, origin, 1)
				called := tracker.snapshot()
				if len(called) == 0 {
					t.Errorf("%s: tool not executed, want auto-execute", tt.name)
				}
			} else {
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
					t.Fatalf("InsertMessage: %v", err)
				}
				if err := h.StartTurn(context.Background(), convID, harness.InboundMessage{Content: "test"}, origin, p); err != nil {
					t.Fatalf("StartTurn: %v", err)
				}
				waitForSuspendOrCompleted(t, bus, 500*time.Millisecond)
			}
		})
	}
}
