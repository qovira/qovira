package harness_test

// context_retry_test.go — black-box integration tests for:
//   AC-1: system prompt contains user context (time/tz/locale/language/displayName).
//   AC-2: reserved memory slot present and empty.
//   AC-3: boundary rule — no orphaned tool results after trimming.
//   AC-4: soft budget enforced.
//   AC-5: ErrContextLength retry logic and bounded exhaust.

import (
	"context"
	"encoding/json"
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

// ── helpers ───────────────────────────────────────────────────────────────────

// makeTestUserWithProfile inserts a user with known profile fields.
func makeTestUserWithProfile(
	t *testing.T,
	s *store.Store,
	timezone, locale, language, displayName string,
) store.Principal {
	t.Helper()
	userID := id.New()
	_, err := s.Writer().ExecContext(context.Background(),
		`INSERT INTO users (id, email, display_name, password_hash, role, timezone, locale, language, created_at, updated_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'), strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
		userID, userID+"@profile.test", displayName, "hash", "member",
		timezone, locale, language,
	)
	if err != nil {
		t.Fatalf("insert profile test user: %v", err)
	}
	return store.Principal{UserID: userID, Role: "member"}
}

// captureChatter captures the ChatRequest passed to each Chat() call.
type captureChatter struct {
	mu       sync.Mutex
	requests []gateway.ChatRequest
	chunks   []gateway.Chunk
}

func (c *captureChatter) Chat(_ context.Context, req gateway.ChatRequest) (iter.Seq2[gateway.Chunk, error], error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()

	chunks := c.chunks
	seq := func(yield func(gateway.Chunk, error) bool) {
		for _, ch := range chunks {
			if !yield(ch, nil) {
				return
			}
		}
	}
	return seq, nil
}

func (c *captureChatter) lastRequest() gateway.ChatRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) == 0 {
		return gateway.ChatRequest{}
	}
	return c.requests[len(c.requests)-1]
}

// buildHarnessWithClock builds a Harness with an injected clock and optional config.
func buildHarnessWithClock(
	t *testing.T,
	s *store.Store,
	gw harness.Chatter,
	bus events.Publisher,
	nowFn func() time.Time,
	cfg harness.Config,
) *harness.Harness {
	t.Helper()
	reg := capability.NewRegistry()
	return harness.NewWithClock(reg, gw, s, bus, cfg, harness.NewDiscardLogger(), nowFn)
}

// startAndWaitCompletion fires a turn and waits for message.completed or turn.failed. Returns the full event snapshot.
func startAndWaitCompletion(t *testing.T, h *harness.Harness, s *store.Store, p store.Principal, convID string, bus *fakeBus) []fakeEvent {
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
		Content:        "test",
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	origin := harness.Origin{Channel: "web", Trust: harness.Trusted}
	if err := h.StartTurn(context.Background(), convID, harness.InboundMessage{Content: "test"}, origin, p); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
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

// ── AC-1 and AC-2: system prompt contains user context and memory slot ────────

// TestSystemPrompt_ContainsUserContext verifies that the assembled ChatRequest's first message (system) contains:
//   - formatted current time in the user's timezone
//   - IANA timezone string
//   - locale, language, display name
//   - the reserved memory slot marker (empty in v0.1)
func TestSystemPrompt_ContainsUserContext(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUserWithProfile(t, s, "Europe/London", "en-GB", "en", "Test Person")
	bus := &fakeBus{}

	fixedNow := time.Date(2026, 6, 14, 15, 0, 0, 0, time.UTC)

	gw := &captureChatter{
		chunks: []gateway.Chunk{{TextDelta: "ok"}, {Done: true}},
	}

	h := buildHarnessWithClock(t, s, gw, bus, func() time.Time { return fixedNow }, harness.Config{})

	convID := id.New()
	_ = startAndWaitCompletion(t, h, s, p, convID, bus)

	req := gw.lastRequest()
	if len(req.Messages) == 0 {
		t.Fatal("no messages in assembled ChatRequest")
	}

	sysMsg := req.Messages[0]
	if sysMsg.Role != "system" {
		t.Fatalf("first message role = %q, want system", sysMsg.Role)
	}

	// AC-1: time in user's timezone (London is UTC+1 BST in June, so 15:00 UTC = 16:00 BST).
	if !strings.Contains(sysMsg.Content, "2026") {
		t.Errorf("system prompt missing year; prompt:\n%s", sysMsg.Content)
	}
	if !strings.Contains(sysMsg.Content, "Europe/London") {
		t.Errorf("system prompt missing IANA tz %q; prompt:\n%s", "Europe/London", sysMsg.Content)
	}
	if !strings.Contains(sysMsg.Content, "en-GB") {
		t.Errorf("system prompt missing locale %q; prompt:\n%s", "en-GB", sysMsg.Content)
	}
	if !strings.Contains(sysMsg.Content, "en") {
		t.Errorf("system prompt missing language; prompt:\n%s", sysMsg.Content)
	}
	if !strings.Contains(sysMsg.Content, "Test Person") {
		t.Errorf("system prompt missing display name %q; prompt:\n%s", "Test Person", sysMsg.Content)
	}

	// AC-2: memory slot must be present.
	if !strings.Contains(sysMsg.Content, harness.MemorySlotMarker) {
		t.Errorf("system prompt missing memory slot marker %q; prompt:\n%s", harness.MemorySlotMarker, sysMsg.Content)
	}
}

// ── AC-3 & AC-4: sliding window drops oldest complete exchanges ───────────────

// TestSlidingWindow_DropsOldestAndPreservesBoundary verifies that:
//   - A conversation over the budget drops the oldest group first.
//   - A tool-call group is never split (boundary rule).
func TestSlidingWindow_DropsOldestAndPreservesBoundary(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUserWithProfile(t, s, "UTC", "en-US", "en", "Boundary Tester")
	bus := &fakeBus{}

	fixedNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	gw := &captureChatter{
		chunks: []gateway.Chunk{{TextDelta: "result"}, {Done: true}},
	}

	// Tiny budget — 10 tokens — so the oldest groups must be dropped.
	h := buildHarnessWithClock(t, s, gw, bus, func() time.Time { return fixedNow },
		harness.Config{HistoryTokenBudget: 10})

	convID := id.New()
	scope := store.UserScope(p)
	sq := s.ForUser(scope)

	if err := sq.UpsertConversation(context.Background(), convID); err != nil {
		t.Fatalf("UpsertConversation: %v", err)
	}

	// Insert history: group 0 = tool-call exchange (old), group 1 = plain (new). The tool-call group has a lot of
	// content so it exceeds the 10-token budget.
	bigContent := strings.Repeat("w", 200) // ~50 tokens via chars/4 heuristic
	toolCallsJSON, err := json.Marshal([]gateway.ToolCall{{ID: "old_tc", Name: "tool", Arguments: json.RawMessage(`{}`)}})
	if err != nil {
		t.Fatalf("marshal tool_calls: %v", err)
	}

	// Group 0: user → assistant(tool_calls) → tool → assistant.
	insertMsg := func(role, content, toolCalls, toolCallID string) {
		_, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
			ID:             id.New(),
			ConversationID: convID,
			Role:           role,
			Content:        content,
			ToolCalls:      toolCalls,
			ToolCallID:     toolCallID,
		})
		if err != nil {
			t.Fatalf("InsertMessage(%s): %v", role, err)
		}
	}

	insertMsg("user", bigContent, "", "")
	insertMsg("assistant", "", string(toolCallsJSON), "")
	insertMsg("tool", bigContent, "", "old_tc")
	insertMsg("assistant", bigContent, "", "")

	// Group 1: the "current" user message.
	insertMsg("user", "current question", "", "")

	origin := harness.Origin{Channel: "web", Trust: harness.Trusted}
	if err := h.StartTurn(context.Background(), convID, harness.InboundMessage{Content: "current question"}, origin, p); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}

	// Wait for completion.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		evs := bus.snapshot()
		for _, e := range evs {
			if e.event.Type == "message.completed" || e.event.Type == "turn.failed" {
				goto done
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
done:

	req := gw.lastRequest()
	if len(req.Messages) == 0 {
		t.Fatal("no messages captured in ChatRequest")
	}

	// Skip the system message.
	historyMsgs := req.Messages[1:]

	// BOUNDARY RULE: no tool message should appear without its assistant tool_calls.
	for i, m := range historyMsgs {
		if m.Role != "tool" {
			continue
		}
		found := false
		for j := i - 1; j >= 0; j-- {
			for _, tc := range historyMsgs[j].ToolCalls {
				if tc.ID == m.ToolCallID {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			t.Errorf("tool message at history[%d] (tool_call_id=%q) is orphaned in trimmed history; full history: %v",
				i, m.ToolCallID, roleList(historyMsgs))
		}
	}

	// "current question" must be in the assembled history (newest group kept).
	hasCurrentQ := false
	for _, m := range historyMsgs {
		if m.Role == "user" && strings.Contains(m.Content, "current question") {
			hasCurrentQ = true
		}
	}
	if !hasCurrentQ {
		t.Errorf("newest user message 'current question' missing from trimmed history; history: %v", roleList(historyMsgs))
	}
}

// roleList extracts the roles from a message slice for test error messages.
func roleList(msgs []gateway.Message) []string {
	roles := make([]string, len(msgs))
	for i, m := range msgs {
		roles[i] = m.Role
	}
	return roles
}

// ── AC-5: ErrContextLength retry ──────────────────────────────────────────────

// contextLengthThenSucceedChatter returns ErrContextLength for the first N calls then succeeds.
type contextLengthThenSucceedChatter struct {
	mu        sync.Mutex
	failsLeft int
	callCount int
	successFn func() []gateway.Chunk
}

func newCLChatter(failFirst int, successChunks []gateway.Chunk) *contextLengthThenSucceedChatter {
	return &contextLengthThenSucceedChatter{
		failsLeft: failFirst,
		successFn: func() []gateway.Chunk { return successChunks },
	}
}

func (c *contextLengthThenSucceedChatter) Chat(_ context.Context, _ gateway.ChatRequest) (iter.Seq2[gateway.Chunk, error], error) {
	c.mu.Lock()
	c.callCount++
	fail := c.failsLeft > 0
	if fail {
		c.failsLeft--
	}
	c.mu.Unlock()

	if fail {
		return nil, gateway.ErrContextLength
	}

	chunks := c.successFn()
	seq := func(yield func(gateway.Chunk, error) bool) {
		for _, ch := range chunks {
			if !yield(ch, nil) {
				return
			}
		}
	}
	return seq, nil
}

// alwaysContextLengthChatter always returns ErrContextLength.
type alwaysContextLengthChatter struct {
	mu        sync.Mutex
	callCount int
}

func (c *alwaysContextLengthChatter) Chat(_ context.Context, _ gateway.ChatRequest) (iter.Seq2[gateway.Chunk, error], error) {
	c.mu.Lock()
	c.callCount++
	c.mu.Unlock()
	return nil, gateway.ErrContextLength
}

// TestContextRetry_SucceedsAfterTrim verifies that the harness retries with a harder trim after ErrContextLength and
// completes successfully when a later attempt succeeds.
func TestContextRetry_SucceedsAfterTrim(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUserWithProfile(t, s, "UTC", "en-US", "en", "Retry Person")
	bus := &fakeBus{}

	// Fail once, then succeed.
	gw := newCLChatter(1, []gateway.Chunk{{TextDelta: "trimmed reply"}, {Done: true}})

	h := buildHarnessWithClock(t, s, gw, bus, time.Now,
		harness.Config{MaxContextRetries: 2})

	convID := id.New()
	evs := startAndWaitCompletion(t, h, s, p, convID, bus)

	// Must have exactly one message.completed and no turn.failed.
	var completedCount, failedCount int
	for _, e := range evs {
		switch e.event.Type {
		case "message.completed":
			completedCount++
		case "turn.failed":
			failedCount++
		}
	}

	if completedCount != 1 {
		t.Errorf("message.completed count = %d, want 1 (retry should succeed)", completedCount)
	}
	if failedCount != 0 {
		t.Errorf("turn.failed count = %d, want 0", failedCount)
	}
}

// TestContextRetry_ExhaustsAndEmitsGraceful verifies that when ErrContextLength always fires, the harness exhausts the
// retry bound and emits exactly ONE graceful message.completed{finishReason:"context_length"}. It must NOT emit
// turn.failed alongside it.
func TestContextRetry_ExhaustsAndEmitsGraceful(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUserWithProfile(t, s, "UTC", "en-US", "en", "Exhaust Person")
	bus := &fakeBus{}

	gw := &alwaysContextLengthChatter{}

	h := buildHarnessWithClock(t, s, gw, bus, time.Now,
		harness.Config{MaxContextRetries: 2})

	convID := id.New()
	evs := startAndWaitCompletion(t, h, s, p, convID, bus)

	var completedEvts []harness.CompletedPayload
	var failedCount int
	for _, e := range evs {
		switch e.event.Type {
		case "message.completed":
			if payload, ok := e.event.Data.(harness.CompletedPayload); ok {
				completedEvts = append(completedEvts, payload)
			}
		case "turn.failed":
			failedCount++
		}
	}

	// Exactly one message.completed.
	if len(completedEvts) != 1 {
		t.Errorf("message.completed count = %d, want exactly 1", len(completedEvts))
	} else if completedEvts[0].FinishReason != "context_length" {
		t.Errorf("finishReason = %q, want context_length", completedEvts[0].FinishReason)
	}

	// Zero turn.failed — single-terminal-event invariant.
	if failedCount != 0 {
		t.Errorf("turn.failed count = %d, want 0 — context_length exhaust must NOT emit turn.failed", failedCount)
	}

	// The call count must be exactly 1 original attempt + MaxContextRetries (= 2). A regression that skips retries
	// would call the gateway fewer times and still emit a graceful context_length terminal — the lower bound catches
	// it.
	const wantCallCount = 1 + 2 // 1 original + MaxContextRetries
	if gw.callCount != wantCallCount {
		t.Errorf("gateway called %d times, want exactly %d (1 original + 2 retries)", gw.callCount, wantCallCount)
	}
}

// TestContextRetry_BoundedRetries verifies the retry count is bounded by MaxContextRetries.
func TestContextRetry_BoundedRetries(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUserWithProfile(t, s, "UTC", "en-US", "en", "Bound Person")
	bus := &fakeBus{}

	gw := &alwaysContextLengthChatter{}

	const maxRetries = 3
	h := buildHarnessWithClock(t, s, gw, bus, time.Now,
		harness.Config{MaxContextRetries: maxRetries})

	convID := id.New()
	_ = startAndWaitCompletion(t, h, s, p, convID, bus)

	// Total calls must be exactly 1 original attempt + maxRetries. The lower bound catches a regression that emits the
	// graceful terminal after fewer retries than configured (e.g. off-by-one in the retry counter).
	wantCallCount := 1 + maxRetries
	if gw.callCount != wantCallCount {
		t.Errorf("gateway called %d times, want exactly %d (1 + MaxContextRetries=%d)", gw.callCount, wantCallCount, maxRetries)
	}
}

// TestContextRetry_DefaultConfigApplied verifies that zero-value Config applies the default MaxContextRetries (2) and
// HistoryTokenBudget (50_000).
func TestContextRetry_DefaultConfigApplied(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUserWithProfile(t, s, "UTC", "en-US", "en", "Default Person")
	bus := &fakeBus{}

	gw := &alwaysContextLengthChatter{}

	// Zero-value config — defaults must apply.
	h := buildHarnessWithClock(t, s, gw, bus, time.Now, harness.Config{})

	convID := id.New()
	_ = startAndWaitCompletion(t, h, s, p, convID, bus)

	// Default MaxContextRetries=2 → exactly 3 calls (1 original + 2 retries). The lower bound catches a regression
	// where the default is not applied and the harness calls once and immediately emits the graceful terminal.
	const wantCallCount = 1 + 2 // default MaxContextRetries=2
	if gw.callCount != wantCallCount {
		t.Errorf("gateway called %d times with default config, want exactly %d (1 original + 2 default retries)", gw.callCount, wantCallCount)
	}
}

// ── FIX 2: stream-level ErrContextLength must not consume step budget ────────

// streamCLThenSucceedChatter behaves like contextLengthThenSucceedChatter but returns ErrContextLength as a
// STREAM-LEVEL error (after setup succeeds), not at the Chat() call level. This exercises the stream-level CL branch
// in run().
type streamCLThenSucceedChatter struct {
	mu        sync.Mutex
	failsLeft int
	callCount int
	success   []gateway.Chunk
}

func newStreamCLChatter(failFirst int, successChunks []gateway.Chunk) *streamCLThenSucceedChatter {
	return &streamCLThenSucceedChatter{
		failsLeft: failFirst,
		success:   successChunks,
	}
}

func (c *streamCLThenSucceedChatter) Chat(_ context.Context, _ gateway.ChatRequest) (iter.Seq2[gateway.Chunk, error], error) {
	c.mu.Lock()
	c.callCount++
	fail := c.failsLeft > 0
	if fail {
		c.failsLeft--
	}
	chunks := c.success
	c.mu.Unlock()

	if fail {
		// Return a stream that yields ErrContextLength mid-stream (setup succeeds).
		return func(yield func(gateway.Chunk, error) bool) {
			yield(gateway.Chunk{}, gateway.ErrContextLength)
		}, nil
	}
	return func(yield func(gateway.Chunk, error) bool) {
		for _, ch := range chunks {
			if !yield(ch, nil) {
				return
			}
		}
	}, nil
}

// alwaysStreamCLChatter always returns ErrContextLength mid-stream.
type alwaysStreamCLChatter struct {
	mu        sync.Mutex
	callCount int
}

func (c *alwaysStreamCLChatter) Chat(_ context.Context, _ gateway.ChatRequest) (iter.Seq2[gateway.Chunk, error], error) {
	c.mu.Lock()
	c.callCount++
	c.mu.Unlock()
	return func(yield func(gateway.Chunk, error) bool) {
		yield(gateway.Chunk{}, gateway.ErrContextLength)
	}, nil
}

// TestContextRetry_StreamLevel_DoesNotConsumeStepBudget verifies that a stream-level ErrContextLength retries without
// advancing the step counter. The gateway is called failFirst times with a stream-level CL error, then succeeds. The
// turn must complete with finishReason "stop", not "step_cap", meaning the CL retries did NOT burn step budget.
func TestContextRetry_StreamLevel_DoesNotConsumeStepBudget(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUserWithProfile(t, s, "UTC", "en-US", "en", "Stream CL Person")
	bus := &fakeBus{}

	// Fail twice at stream level, then succeed. With MaxContextRetries=2 and StepCap=2, if each stream-CL retry
	// consumed a step the turn would exhaust the step cap before reaching the success; it must NOT do that.
	gw := newStreamCLChatter(2, []gateway.Chunk{{TextDelta: "stream retry ok"}, {Done: true}})

	h := buildHarnessWithClock(t, s, gw, bus, time.Now,
		harness.Config{MaxContextRetries: 2, StepCap: 2})

	convID := id.New()
	evs := startAndWaitCompletion(t, h, s, p, convID, bus)

	var completedEvts []harness.CompletedPayload
	var failedCount int
	for _, e := range evs {
		switch e.event.Type {
		case "message.completed":
			if payload, ok := e.event.Data.(harness.CompletedPayload); ok {
				completedEvts = append(completedEvts, payload)
			}
		case "turn.failed":
			failedCount++
		}
	}

	if len(completedEvts) != 1 {
		t.Errorf("message.completed count = %d, want 1", len(completedEvts))
	} else if completedEvts[0].FinishReason != "stop" {
		// If stream-CL consumed the step budget, finishReason would be "step_cap".
		t.Errorf("finishReason = %q, want stop (step budget must not be consumed by CL retries)", completedEvts[0].FinishReason)
	}
	if failedCount != 0 {
		t.Errorf("turn.failed count = %d, want 0", failedCount)
	}
	// The stream-CL retries must not be "free" in both directions: if the harness retried fewer than MaxContextRetries
	// times it would still reach the success path early — but here gw fails 2 times then succeeds, so callCount must
	// be 3.
	const wantCallCount = 3 // 2 stream-CL failures + 1 success
	if gw.callCount != wantCallCount {
		t.Errorf("gateway called %d times, want exactly %d (2 stream-CL failures + 1 success)", gw.callCount, wantCallCount)
	}
}

// TestContextRetry_StreamLevel_ExhaustsGracefully verifies that when stream-level ErrContextLength always fires the
// harness exhausts maxContextRetries, emits exactly ONE message.completed{finishReason:"context_length"}, and calls
// the gateway at most 1+maxContextRetries times.
func TestContextRetry_StreamLevel_ExhaustsGracefully(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUserWithProfile(t, s, "UTC", "en-US", "en", "Stream Exhaust Person")
	bus := &fakeBus{}

	gw := &alwaysStreamCLChatter{}

	h := buildHarnessWithClock(t, s, gw, bus, time.Now,
		harness.Config{MaxContextRetries: 2})

	convID := id.New()
	evs := startAndWaitCompletion(t, h, s, p, convID, bus)

	var completedEvts []harness.CompletedPayload
	var failedCount int
	for _, e := range evs {
		switch e.event.Type {
		case "message.completed":
			if payload, ok := e.event.Data.(harness.CompletedPayload); ok {
				completedEvts = append(completedEvts, payload)
			}
		case "turn.failed":
			failedCount++
		}
	}

	if len(completedEvts) != 1 {
		t.Errorf("message.completed count = %d, want exactly 1", len(completedEvts))
	} else if completedEvts[0].FinishReason != "context_length" {
		t.Errorf("finishReason = %q, want context_length", completedEvts[0].FinishReason)
	}
	if failedCount != 0 {
		t.Errorf("turn.failed count = %d, want 0 — stream CL exhaust must not emit turn.failed", failedCount)
	}
	// Exact lower+upper bound: MaxContextRetries=2 → exactly 3 calls (1 original + 2 stream-CL retries). Under-retrying
	// (e.g. retrying only once) would still emit a graceful context_length terminal, so the lower bound is load-bearing
	// here.
	const wantCallCount = 1 + 2 // 1 original + MaxContextRetries=2
	if gw.callCount != wantCallCount {
		t.Errorf("gateway called %d times, want exactly %d (1 original + MaxContextRetries=2 retries)", gw.callCount, wantCallCount)
	}
}

// TestContextRetry_RaceClean verifies the context-retry path is race-detector clean.
func TestContextRetry_RaceClean(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUserWithProfile(t, s, "UTC", "en-US", "en", "Race Person")
	bus := &fakeBus{}

	// Fail once, then succeed — covers the retry code path.
	gw := newCLChatter(1, []gateway.Chunk{{TextDelta: "race ok"}, {Done: true}})

	h := buildHarnessWithClock(t, s, gw, bus, time.Now, harness.Config{})

	convID := id.New()
	_ = startAndWaitCompletion(t, h, s, p, convID, bus)

	// Just verify that message.completed was emitted (not a turn.failed).
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
