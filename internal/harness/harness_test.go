package harness_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
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

// openMigratedStore opens a temporary encrypted store and runs all migrations.
func openMigratedStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(store.Config{
		Path: filepath.Join(dir, "test.db"),
		Key:  "test-key-sufficiently-long-for-sqlcipher",
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	runner := store.NewRunner()
	if err := runner.Up(context.Background(), s.Writer()); err != nil {
		_ = s.Close()
		t.Fatalf("migrations.Up: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// makeTestUser inserts a minimal user row and returns the Principal.
func makeTestUser(t *testing.T, s *store.Store) store.Principal {
	t.Helper()
	userID := id.New()
	_, err := s.Writer().ExecContext(context.Background(),
		`INSERT INTO users (id, email, display_name, password_hash, role, timezone, locale, language, created_at, updated_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'), strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
		userID, userID+"@test.example", "Test User", "hash", "member", "UTC", "en-US", "en",
	)
	if err != nil {
		t.Fatalf("insert test user: %v", err)
	}
	return store.Principal{UserID: userID, Role: "member"}
}

// fakeChatter implements harness.Chatter, returning a canned sequence of chunks.
type fakeChatter struct {
	chunks []gateway.Chunk
	err    error // if non-nil and no chunks, setup error; otherwise stream error after chunks
}

func (f *fakeChatter) Chat(_ context.Context, _ gateway.ChatRequest) (iter.Seq2[gateway.Chunk, error], error) {
	if f.err != nil && len(f.chunks) == 0 {
		return nil, f.err
	}
	chunks := f.chunks
	streamErr := f.err
	seq := func(yield func(gateway.Chunk, error) bool) {
		for _, c := range chunks {
			if !yield(c, nil) {
				return
			}
		}
		if streamErr != nil {
			yield(gateway.Chunk{}, streamErr)
		}
	}
	return seq, nil
}

// fakeEvent records one published event.
type fakeEvent struct {
	userID string
	event  events.Event
}

// fakeBus captures published events in order, safe for concurrent use.
type fakeBus struct {
	mu     sync.Mutex
	events []fakeEvent
}

func (b *fakeBus) Publish(userID string, e events.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, fakeEvent{userID: userID, event: e})
}

func (b *fakeBus) snapshot() []fakeEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]fakeEvent, len(b.events))
	copy(out, b.events)
	return out
}

// waitForEvents blocks until the bus has at least n events or the timeout expires.
// Prefer waitForCompleted when testing turn completion — a raw event count is
// satisfied mid-turn and misses the terminal message.completed event.
func waitForEvents(t *testing.T, b *fakeBus, n int, timeout time.Duration) []fakeEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if evs := b.snapshot(); len(evs) >= n {
			return evs
		}
		time.Sleep(2 * time.Millisecond)
	}
	return b.snapshot()
}

// waitForCompleted blocks until a "message.completed" event is present on the
// bus (and, optionally, until at least minToolStarted "tool.started" events
// have also appeared). It returns the full event snapshot once the condition is
// met, or the snapshot at timeout. Using this instead of waitForEvents avoids
// sampling the bus mid-turn before the terminal event fires.
func waitForCompleted(t *testing.T, b *fakeBus, minToolStarted int, timeout time.Duration) []fakeEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		evs := b.snapshot()
		completedCount := 0
		toolStartedCount := 0
		for _, e := range evs {
			switch e.event.Type {
			case "message.completed":
				completedCount++
			case "tool.started":
				toolStartedCount++
			}
		}
		if completedCount >= 1 && toolStartedCount >= minToolStarted {
			return evs
		}
		time.Sleep(2 * time.Millisecond)
	}
	return b.snapshot()
}

// buildHarness constructs a *harness.Harness from fakes.
func buildHarness(t *testing.T, s *store.Store, gw harness.Chatter, bus events.Publisher) *harness.Harness {
	t.Helper()
	logger := harness.NewDiscardLogger()
	reg := capability.NewRegistry()
	return harness.New(reg, gw, s, bus, harness.Config{}, logger)
}

// buildServer builds a bare http.Server with the harness routes mounted and no
// auth middleware (tests inject the principal directly into context).
func buildServer(h *harness.Harness) *http.Server {
	router := httpx.NewRouter()
	h.Routes(router)
	return httpx.NewServer("127.0.0.1:0", "test", router, events.NewBus())
}

// makeAuthedRequest builds a request with the Principal injected into context (simulating the auth middleware).
func makeAuthedRequest(method, path string, body []byte, p store.Principal) *http.Request {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("Authorization", "Bearer test-token")
	ctx := httpx.ContextWithPrincipal(r.Context(), p)
	return r.WithContext(ctx)
}

// ── AC-1: POST persists user message and returns 202 ─────────────────────────

func TestPostMessage_PersistsUserMessageAndReturns202(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	gw := &fakeChatter{chunks: []gateway.Chunk{{TextDelta: "hello"}, {Done: true}}}
	h := buildHarness(t, s, gw, bus)
	srv := buildServer(h)

	convID := id.New()
	body, _ := json.Marshal(map[string]string{"content": "what is 2+2?"})
	req := makeAuthedRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", body, p)

	rr := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST /messages: status = %d, want 202; body: %s", rr.Code, rr.Body.String())
	}

	ct := rr.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var msg harness.MessageResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &msg); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if msg.ID == "" {
		t.Error("response id is empty")
	}
	if msg.Role != "user" {
		t.Errorf("response role = %q, want %q", msg.Role, "user")
	}
	if msg.Content != "what is 2+2?" {
		t.Errorf("response content = %q, want %q", msg.Content, "what is 2+2?")
	}
	if msg.ConversationID != convID {
		t.Errorf("response conversationId = %q, want %q", msg.ConversationID, convID)
	}

	// Verify persisted in store.
	scope := store.UserScope(p)
	sq := s.ForUser(scope)
	msgs, err := sq.ListMessages(context.Background(), convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("no messages persisted after POST")
	}
	if msgs[0].Role != "user" {
		t.Errorf("persisted role = %q, want user", msgs[0].Role)
	}
	if msgs[0].Content != "what is 2+2?" {
		t.Errorf("persisted content = %q, want %q", msgs[0].Content, "what is 2+2?")
	}
}

// ── AC-2: text-only reply streams ordered message.delta + one message.completed ──

func TestRun_TextOnlyReply_StreamsEventsInOrder(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	gw := &fakeChatter{chunks: []gateway.Chunk{
		{TextDelta: "tok1"},
		{TextDelta: "tok2"},
		{TextDelta: "tok3"},
		{Done: true},
	}}
	h := buildHarness(t, s, gw, bus)
	srv := buildServer(h)

	convID := id.New()
	body, _ := json.Marshal(map[string]string{"content": "hello"})
	req := makeAuthedRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", body, p)
	rr := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST /messages: status = %d; body: %s", rr.Code, rr.Body.String())
	}

	// Wait for turn goroutine to publish all events (3 deltas + 1 completed).
	evs := waitForEvents(t, bus, 4, 3*time.Second)

	var deltaEvts []fakeEvent
	var completedEvts []fakeEvent
	for _, e := range evs {
		switch e.event.Type {
		case "message.delta":
			deltaEvts = append(deltaEvts, e)
		case "message.completed":
			completedEvts = append(completedEvts, e)
		}
	}

	if len(deltaEvts) != 3 {
		t.Errorf("message.delta count = %d, want 3", len(deltaEvts))
	}
	if len(completedEvts) != 1 {
		t.Errorf("message.completed count = %d, want 1", len(completedEvts))
	}

	// All deltas must come before message.completed.
	if len(evs) >= 4 {
		last := evs[len(evs)-1]
		if last.event.Type != "message.completed" {
			t.Errorf("last event type = %q, want message.completed", last.event.Type)
		}
	}

	// Delta payloads must carry conversationId and text.
	if len(deltaEvts) > 0 {
		payload, ok := deltaEvts[0].event.Data.(harness.DeltaPayload)
		if !ok {
			t.Errorf("delta Data type = %T, want harness.DeltaPayload", deltaEvts[0].event.Data)
		} else if payload.ConversationID != convID {
			t.Errorf("delta conversationId = %q, want %q", payload.ConversationID, convID)
		}
	}

	// Verify delta text ordering.
	wantTexts := []string{"tok1", "tok2", "tok3"}
	for i, e := range deltaEvts {
		if i >= len(wantTexts) {
			break
		}
		payload, ok := e.event.Data.(harness.DeltaPayload)
		if !ok {
			continue
		}
		if payload.Text != wantTexts[i] {
			t.Errorf("delta[%d].text = %q, want %q", i, payload.Text, wantTexts[i])
		}
	}

	// Completed payload must carry messageId and finishReason.
	if len(completedEvts) == 1 {
		payload, ok := completedEvts[0].event.Data.(harness.CompletedPayload)
		if !ok {
			t.Errorf("completed Data type = %T, want harness.CompletedPayload", completedEvts[0].event.Data)
		} else {
			if payload.MessageID == "" {
				t.Error("completed payload messageId is empty")
			}
			if payload.FinishReason == "" {
				t.Error("completed payload finishReason is empty")
			}
		}
	}
}

// ── AC-3: assistant message persisted BEFORE message.completed ────────────────

func TestRun_AssistantPersistedBeforeCompleted(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)

	// A bus that, on message.completed, checks that the assistant message is
	// already in the DB.
	assertBus := &assertPersistBus{t: t, s: s, userID: p.UserID}

	gw := &fakeChatter{chunks: []gateway.Chunk{
		{TextDelta: "hello world"},
		{Done: true},
	}}
	reg := capability.NewRegistry()
	h := harness.New(reg, gw, s, assertBus, harness.Config{}, harness.NewDiscardLogger())
	srv := buildServer(h)

	convID := id.New()
	body, _ := json.Marshal(map[string]string{"content": "hi"})
	req := makeAuthedRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", body, p)
	rr := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d; body: %s", rr.Code, rr.Body.String())
	}

	// Wait until message.completed was received.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if assertBus.gotCompleted() {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	if !assertBus.gotCompleted() {
		t.Fatal("message.completed event never received")
	}
	if assertBus.violated() {
		t.Error("assistant message was NOT persisted before message.completed was emitted")
	}
}

// assertPersistBus checks that assistant message is in DB when message.completed fires.
type assertPersistBus struct {
	t      *testing.T
	s      *store.Store
	userID string
	mu     sync.Mutex
	done   bool
	viol   bool
}

func (b *assertPersistBus) Publish(userID string, e events.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e.Type != "message.completed" {
		return
	}
	payload, ok := e.Data.(harness.CompletedPayload)
	if !ok {
		b.done = true
		return
	}
	var n int
	err := b.s.Reader().QueryRowContext(
		context.Background(),
		`SELECT count(*) FROM messages WHERE id = ? AND user_id = ? AND role = 'assistant'`,
		payload.MessageID, userID,
	).Scan(&n)
	if err != nil || n == 0 {
		b.viol = true
	}
	b.done = true
}

func (b *assertPersistBus) gotCompleted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.done
}

func (b *assertPersistBus) violated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.viol
}

// ── AC-4: StartTurn returns before the turn finishes ─────────────────────────

func TestStartTurn_ReturnsBeforeTurnFinishes(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}

	ready := make(chan struct{})
	gw := &blockingChatter{ready: ready}

	reg := capability.NewRegistry()
	h := harness.New(reg, gw, s, bus, harness.Config{}, harness.NewDiscardLogger())

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
		Content:        "slow question",
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	msg := harness.InboundMessage{Content: "slow question"}
	origin := harness.Origin{Channel: "web", Trust: harness.Trusted}

	done := make(chan error, 1)
	go func() {
		done <- h.StartTurn(context.Background(), convID, msg, origin, p)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("StartTurn returned error: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("StartTurn blocked >500ms; must return before turn finishes")
	}

	// Release gateway so turn goroutine can finish without leaking.
	close(ready)
	_ = waitForEvents(t, bus, 1, 2*time.Second)
}

// blockingChatter blocks until ready is closed, then yields Done.
type blockingChatter struct {
	ready <-chan struct{}
}

func (b *blockingChatter) Chat(ctx context.Context, _ gateway.ChatRequest) (iter.Seq2[gateway.Chunk, error], error) {
	seq := func(yield func(gateway.Chunk, error) bool) {
		select {
		case <-b.ready:
		case <-ctx.Done():
			yield(gateway.Chunk{}, ctx.Err())
			return
		}
		yield(gateway.Chunk{Done: true}, nil)
	}
	return seq, nil
}

// ── AC-5: Trust resolves to Trusted; Origin carries Channel:"web" ─────────────

func TestResolveTrust_ReturnsWebTrusted(t *testing.T) {
	t.Parallel()

	origin := harness.ResolveOrigin()
	if origin.Trust != harness.Trusted {
		t.Errorf("trust = %v, want Trusted", origin.Trust)
	}
	if origin.Channel != "web" {
		t.Errorf("channel = %q, want \"web\"", origin.Channel)
	}
}

// ── AC-6 (race): table-driven test exercising concurrent turns ───────────────

func TestPostMessage_RaceClean(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
	}{
		{name: "simple", content: "hello"},
		{name: "question", content: "what is the capital of France?"},
		{name: "multi_word", content: "tell me more about Go"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := openMigratedStore(t)
			p := makeTestUser(t, s)
			bus := &fakeBus{}
			gw := &fakeChatter{chunks: []gateway.Chunk{{TextDelta: "ok"}, {Done: true}}}
			h := buildHarness(t, s, gw, bus)
			srv := buildServer(h)

			convID := id.New()
			body, _ := json.Marshal(map[string]string{"content": tt.content})
			req := makeAuthedRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", body, p)
			rr := httptest.NewRecorder()
			srv.Handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusAccepted {
				t.Errorf("status = %d; body: %s", rr.Code, rr.Body.String())
			}

			// Wait for the turn goroutine to complete so the race detector observes all accesses.
			waitForEvents(t, bus, 2, 2*time.Second)
		})
	}
}

// ── AC-4 supplement: turn output does not appear in POST response body ────────

func TestPostMessage_ResponseBodyDoesNotContainTurnOutput(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	gw := &fakeChatter{chunks: []gateway.Chunk{{TextDelta: "SECRET TURN OUTPUT"}, {Done: true}}}
	h := buildHarness(t, s, gw, bus)
	srv := buildServer(h)

	convID := id.New()
	body, _ := json.Marshal(map[string]string{"content": "hi"})
	req := makeAuthedRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", body, p)
	rr := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d; body: %s", rr.Code, rr.Body.String())
	}

	if strings.Contains(rr.Body.String(), "SECRET TURN OUTPUT") {
		t.Error("POST response body contained turn output; it must not")
	}

	// Allow turn goroutine to finish so the race detector sees it.
	waitForEvents(t, bus, 2, 2*time.Second)
}

// ── upsert conversation gap-fill ─────────────────────────────────────────────

func TestPostMessage_UpsertConversation(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	gw := &fakeChatter{chunks: []gateway.Chunk{{Done: true}}}
	h := buildHarness(t, s, gw, bus)
	srv := buildServer(h)

	convID := id.New()

	// First message: creates the conversation.
	body, _ := json.Marshal(map[string]string{"content": "message 1"})
	req := makeAuthedRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", body, p)
	rr := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("first POST status = %d; body: %s", rr.Code, rr.Body.String())
	}

	// Verify conversation row created.
	scope := store.UserScope(p)
	sq := s.ForUser(scope)
	_, err := sq.GetConversation(context.Background(), convID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			t.Fatal("conversation was not created by first POST")
		}
		t.Fatalf("GetConversation: %v", err)
	}

	// Second message on same conversation.
	body2, _ := json.Marshal(map[string]string{"content": "message 2"})
	req2 := makeAuthedRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", body2, p)
	rr2 := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusAccepted {
		t.Fatalf("second POST status = %d; body: %s", rr2.Code, rr2.Body.String())
	}

	// Wait for both turn goroutines to complete.
	waitForEvents(t, bus, 2, 3*time.Second)

	msgs, err := sq.ListMessages(context.Background(), convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	userMsgs := 0
	for _, m := range msgs {
		if m.Role == "user" {
			userMsgs++
		}
	}
	if userMsgs < 2 {
		t.Errorf("user message count = %d, want >= 2", userMsgs)
	}
}

// ── MUST-FIX 1: cross-user conversation ownership guard ───────────────────────

// TestPostMessage_CrossUserConversationRejected verifies the cross-user data-breach
// fix: user B must be rejected (404) when POSTing into a conversation id that already
// belongs to user A. A's conversation must remain intact; B's message must NOT be
// persisted.
func TestPostMessage_CrossUserConversationRejected(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	// Two distinct users in the same store.
	pA := makeTestUser(t, s)
	pB := makeTestUser(t, s)
	bus := &fakeBus{}
	gw := &fakeChatter{chunks: []gateway.Chunk{{Done: true}}}

	// Each user needs their own harness/server because the handler resolves the
	// principal from the injected context — build one shared harness (the store is
	// shared) and inject principals per-request.
	h := buildHarness(t, s, gw, bus)
	srv := buildServer(h)

	convID := id.New()

	// User A creates the conversation with their first message.
	bodyA, _ := json.Marshal(map[string]string{"content": "user A message"})
	reqA := makeAuthedRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", bodyA, pA)
	rrA := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rrA, reqA)
	if rrA.Code != http.StatusAccepted {
		t.Fatalf("user A POST: status = %d, want 202; body: %s", rrA.Code, rrA.Body.String())
	}

	// Wait for A's turn goroutine so the store is quiescent before B tries.
	waitForEvents(t, bus, 1, 3*time.Second)

	// User B attempts to POST into the same conversation id (owned by A).
	bodyB, _ := json.Marshal(map[string]string{"content": "user B intrusion"})
	reqB := makeAuthedRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", bodyB, pB)
	rrB := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rrB, reqB)

	// Must be rejected with 404 (not-found, to avoid leaking that the conversation exists).
	if rrB.Code != http.StatusNotFound {
		t.Errorf("user B POST into A's conversation: status = %d, want 404; body: %s", rrB.Code, rrB.Body.String())
	}

	// A's conversation must still contain only A's messages — no cross-user contamination.
	sqA := s.ForUser(store.UserScope(pA))
	msgsA, err := sqA.ListMessages(context.Background(), convID)
	if err != nil {
		t.Fatalf("ListMessages for user A: %v", err)
	}
	for _, m := range msgsA {
		if strings.Contains(m.Content, "user B intrusion") {
			t.Error("user B's message content found in user A's conversation — cross-user breach")
		}
	}

	// B's message must NOT appear under B's scoped view of that conversation either.
	sqB := s.ForUser(store.UserScope(pB))
	msgsB, err := sqB.ListMessages(context.Background(), convID)
	if err != nil {
		t.Fatalf("ListMessages for user B: %v", err)
	}
	if len(msgsB) != 0 {
		t.Errorf("user B has %d messages in conversation %s, want 0", len(msgsB), convID)
	}
}
