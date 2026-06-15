package harness_test

// conversations_test.go — handler tests for GET /api/v1/conversations and
// GET /api/v1/conversations/{id} (read-only conversation endpoints).
//
// TDD: these tests are written before the handlers exist. They assert the
// acceptance criteria from the conversation read API issue.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/gateway"
	"github.com/qovira/qovira/internal/harness"
	"github.com/qovira/qovira/internal/httpx"
	"github.com/qovira/qovira/internal/id"
	"github.com/qovira/qovira/internal/store"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// seedConversation creates a conversation for the given user with one user message
// and one assistant message, and returns the conversation ID.
func seedConversation(t *testing.T, s *store.Store, p store.Principal, userContent string) string {
	t.Helper()
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
		Content:        userContent,
	}); err != nil {
		t.Fatalf("InsertMessage user: %v", err)
	}
	return convID
}

// seedConversationWithMessages creates a conversation with multiple messages and
// returns the conversation ID.
func seedConversationWithMessages(t *testing.T, s *store.Store, p store.Principal) string {
	t.Helper()
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
		Content:        "hello",
	}); err != nil {
		t.Fatalf("InsertMessage user: %v", err)
	}
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             id.New(),
		ConversationID: convID,
		Role:           "assistant",
		Content:        "world",
		FinishReason:   "stop",
	}); err != nil {
		t.Fatalf("InsertMessage assistant: %v", err)
	}
	return convID
}

// getConversations sends GET /api/v1/conversations with optional query string.
func getConversations(h *harness.Harness, p store.Principal, query string) *httptest.ResponseRecorder {
	path := "/api/v1/conversations"
	if query != "" {
		path += "?" + query
	}
	req := makeAuthedRequest(http.MethodGet, path, nil, p)
	rr := httptest.NewRecorder()
	router := httpx.NewRouter()
	h.Routes(router)
	router.ServeHTTP(rr, req)
	return rr
}

// getConversationHistory sends GET /api/v1/conversations/{id}.
func getConversationHistory(h *harness.Harness, p store.Principal, convID string) *httptest.ResponseRecorder {
	req := makeAuthedRequest(http.MethodGet, "/api/v1/conversations/"+convID, nil, p)
	rr := httptest.NewRecorder()
	router := httpx.NewRouter()
	h.Routes(router)
	router.ServeHTTP(rr, req)
	return rr
}

// ── Part 1: GET /api/v1/conversations ────────────────────────────────────────

// TestListConversations_401_Unauthenticated verifies that unauthenticated
// requests receive 401.
func TestListConversations_401_Unauthenticated(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	bus := &fakeBus{}
	gw := &fakeChatter{}
	h := buildHarness(t, s, gw, bus)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations", nil)
	rr := httptest.NewRecorder()
	router := httpx.NewRouter()
	h.Routes(router)
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body: %s", rr.Code, rr.Body.String())
	}
}

// TestListConversations_UnknownQueryParam_400 verifies that unknown query
// parameters are rejected with 400 unknown_query_param.
func TestListConversations_UnknownQueryParam_400(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	gw := &fakeChatter{}
	h := buildHarness(t, s, gw, bus)

	rr := getConversations(h, p, "unknown=foo")

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body: %s", rr.Code, rr.Body.String())
	}

	var prob map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &prob); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if code, _ := prob["code"].(string); code != "unknown_query_param" {
		t.Errorf("problem code = %q, want unknown_query_param", code)
	}
}

// TestListConversations_OnlyCallerConversations verifies that each user only
// sees their own conversations.
func TestListConversations_OnlyCallerConversations(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	user1 := makeTestUser(t, s)
	user2 := makeTestUser(t, s)
	bus := &fakeBus{}
	gw := &fakeChatter{}
	h := buildHarness(t, s, gw, bus)

	// user1 has one conversation; user2 has two.
	conv1 := seedConversation(t, s, user1, "user1 message")
	_ = seedConversation(t, s, user2, "user2 first")
	_ = seedConversation(t, s, user2, "user2 second")

	rr := getConversations(h, user1, "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}

	var items []map[string]any
	if err := json.Unmarshal(envelope["data"], &items); err != nil {
		t.Fatalf("decode data: %v", err)
	}

	if len(items) != 1 {
		t.Errorf("len(items) = %d, want 1", len(items))
	}
	if len(items) > 0 {
		if got, _ := items[0]["id"].(string); got != conv1 {
			t.Errorf("item[0].id = %q, want %q", got, conv1)
		}
	}
}

// TestListConversations_OrderMostRecentFirst verifies that the list is ordered
// by most-recently-active first (updated_at DESC, id DESC).
func TestListConversations_OrderMostRecentFirst(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	gw := &fakeChatter{chunks: []gateway.Chunk{{TextDelta: "hi"}, {Done: true}}}
	h := buildHarness(t, s, gw, bus)

	// Create two conversations. Touch the first one last so it has a newer updated_at.
	conv1 := seedConversation(t, s, p, "first")
	time.Sleep(10 * time.Millisecond) // ensure different updated_at
	conv2 := seedConversation(t, s, p, "second")
	time.Sleep(10 * time.Millisecond)

	// Touch conv1 to make it most-recently-active.
	scope := store.UserScope(p)
	sq := s.ForUser(scope)
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             id.New(),
		ConversationID: conv1,
		Role:           "user",
		Content:        "follow-up",
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}
	// UpsertConversation touches updated_at.
	if err := sq.UpsertConversation(context.Background(), conv1); err != nil {
		t.Fatalf("UpsertConversation: %v", err)
	}

	rr := getConversations(h, p, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	var items []map[string]any
	if err := json.Unmarshal(envelope["data"], &items); err != nil {
		t.Fatalf("decode data: %v", err)
	}

	if len(items) < 2 {
		t.Fatalf("len(items) = %d, want >= 2", len(items))
	}
	first, _ := items[0]["id"].(string)
	second, _ := items[1]["id"].(string)
	if first != conv1 {
		t.Errorf("items[0].id = %q, want %q (most recently active)", first, conv1)
	}
	if second != conv2 {
		t.Errorf("items[1].id = %q, want %q", second, conv2)
	}
}

// TestListConversations_Pagination verifies hasMore and nextCursor.
func TestListConversations_Pagination(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	gw := &fakeChatter{}
	h := buildHarness(t, s, gw, bus)

	// Create 3 conversations.
	for range 3 {
		_ = seedConversation(t, s, p, "msg")
		time.Sleep(2 * time.Millisecond) // distinct updated_at
	}

	// Fetch page 1 with limit=2.
	rr := getConversations(h, p, "limit=2")
	if rr.Code != http.StatusOK {
		t.Fatalf("page1 status = %d; body: %s", rr.Code, rr.Body.String())
	}

	var page1 map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &page1); err != nil {
		t.Fatalf("decode page1: %v", err)
	}
	var page1Items []map[string]any
	if err := json.Unmarshal(page1["data"], &page1Items); err != nil {
		t.Fatalf("decode page1 data: %v", err)
	}
	var page1Pagination map[string]any
	if err := json.Unmarshal(page1["pagination"], &page1Pagination); err != nil {
		t.Fatalf("decode page1 pagination: %v", err)
	}

	if len(page1Items) != 2 {
		t.Errorf("page1 len = %d, want 2", len(page1Items))
	}
	if hasMore, _ := page1Pagination["hasMore"].(bool); !hasMore {
		t.Error("page1 hasMore = false, want true")
	}
	nextCursor, _ := page1Pagination["nextCursor"].(string)
	if nextCursor == "" {
		t.Error("page1 nextCursor is empty, want non-empty")
	}

	// Fetch page 2 using cursor.
	rr2 := getConversations(h, p, "limit=2&cursor="+nextCursor)
	if rr2.Code != http.StatusOK {
		t.Fatalf("page2 status = %d; body: %s", rr2.Code, rr2.Body.String())
	}

	var page2 map[string]json.RawMessage
	if err := json.Unmarshal(rr2.Body.Bytes(), &page2); err != nil {
		t.Fatalf("decode page2: %v", err)
	}
	var page2Items []map[string]any
	if err := json.Unmarshal(page2["data"], &page2Items); err != nil {
		t.Fatalf("decode page2 data: %v", err)
	}
	var page2Pagination map[string]any
	if err := json.Unmarshal(page2["pagination"], &page2Pagination); err != nil {
		t.Fatalf("decode page2 pagination: %v", err)
	}

	if len(page2Items) != 1 {
		t.Errorf("page2 len = %d, want 1", len(page2Items))
	}
	if hasMore, _ := page2Pagination["hasMore"].(bool); hasMore {
		t.Error("page2 hasMore = true, want false")
	}
}

// TestListConversations_PreviewIsFirstUserMessage verifies that the preview
// field is the first user message content (truncated if long).
func TestListConversations_PreviewIsFirstUserMessage(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	gw := &fakeChatter{}
	h := buildHarness(t, s, gw, bus)

	// Create conversation with known first user message.
	firstMsg := "The quick brown fox jumps over the lazy dog"
	convID := seedConversation(t, s, p, firstMsg)

	// Add a second user message that should NOT appear in preview.
	scope := store.UserScope(p)
	sq := s.ForUser(scope)
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             id.New(),
		ConversationID: convID,
		Role:           "user",
		Content:        "second user message",
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	rr := getConversations(h, p, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rr.Code, rr.Body.String())
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	var items []map[string]any
	if err := json.Unmarshal(envelope["data"], &items); err != nil {
		t.Fatalf("decode data: %v", err)
	}

	if len(items) == 0 {
		t.Fatal("no items")
	}
	preview, _ := items[0]["preview"].(string)
	// preview is either the full first message or truncated version.
	if !strings.HasPrefix(firstMsg, preview) && !strings.HasPrefix(preview, firstMsg[:min(len(firstMsg), 10)]) {
		// The preview should start with the beginning of the first message.
		t.Errorf("preview = %q, want something starting with first user message content", preview)
	}
	// preview must NOT be the second user message.
	if preview == "second user message" {
		t.Error("preview is the second user message, want the first")
	}
}

// TestListConversations_PreviewTruncated verifies that long first messages
// are truncated with ellipsis.
func TestListConversations_PreviewTruncated(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	gw := &fakeChatter{}
	h := buildHarness(t, s, gw, bus)

	// A message longer than the preview cap (80 chars).
	longMsg := strings.Repeat("a", 120)
	_ = seedConversation(t, s, p, longMsg)

	rr := getConversations(h, p, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rr.Code, rr.Body.String())
	}

	var envelope map[string]json.RawMessage
	_ = json.Unmarshal(rr.Body.Bytes(), &envelope)
	var items []map[string]any
	_ = json.Unmarshal(envelope["data"], &items)

	if len(items) == 0 {
		t.Fatal("no items")
	}
	preview, _ := items[0]["preview"].(string)
	if len([]rune(preview)) > 83 { // 80 chars + "..." = 83
		t.Errorf("preview len = %d runes, want <= 83 (80 + ellipsis)", len([]rune(preview)))
	}
	if !strings.HasSuffix(preview, "...") {
		t.Errorf("preview = %q, want suffix '...' for truncated message", preview)
	}
}

// TestListConversations_EmptyPreview verifies that preview is empty when
// no user message exists yet.
func TestListConversations_EmptyPreview(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	gw := &fakeChatter{}
	h := buildHarness(t, s, gw, bus)

	// Create a conversation with only an assistant message (no user message).
	convID := id.New()
	scope := store.UserScope(p)
	sq := s.ForUser(scope)
	if err := sq.UpsertConversation(context.Background(), convID); err != nil {
		t.Fatalf("UpsertConversation: %v", err)
	}
	// Insert only an assistant message.
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             id.New(),
		ConversationID: convID,
		Role:           "assistant",
		Content:        "assistant reply",
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	rr := getConversations(h, p, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rr.Code, rr.Body.String())
	}

	var envelope map[string]json.RawMessage
	_ = json.Unmarshal(rr.Body.Bytes(), &envelope)
	var items []map[string]any
	_ = json.Unmarshal(envelope["data"], &items)

	if len(items) == 0 {
		t.Fatal("no items")
	}
	preview, _ := items[0]["preview"].(string)
	if preview != "" {
		t.Errorf("preview = %q, want empty string when no user message exists", preview)
	}
}

// TestListConversations_InvalidCursor_400 verifies that a malformed cursor is
// rejected with 400 invalid_cursor (not a 500) so a corrupted/forged cursor is a
// clean client error, never an internal failure.
func TestListConversations_InvalidCursor_400(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	gw := &fakeChatter{}
	h := buildHarness(t, s, gw, bus)

	rr := getConversations(h, p, "cursor=%21%21not-base64%21%21")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rr.Code, rr.Body.String())
	}
	var prob map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &prob); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if code, _ := prob["code"].(string); code != "invalid_cursor" {
		t.Errorf("problem code = %q, want invalid_cursor", code)
	}
}

// ── Part 2: GET /api/v1/conversations/{id} ────────────────────────────────────

// TestGetConversationHistory_401_Unauthenticated verifies 401 for unauthed requests.
func TestGetConversationHistory_401_Unauthenticated(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	bus := &fakeBus{}
	gw := &fakeChatter{}
	h := buildHarness(t, s, gw, bus)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+id.New(), nil)
	rr := httptest.NewRecorder()
	router := httpx.NewRouter()
	h.Routes(router)
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

// TestGetConversationHistory_404_OtherUser verifies 404 when the conversation
// belongs to another user.
func TestGetConversationHistory_404_OtherUser(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	owner := makeTestUser(t, s)
	other := makeTestUser(t, s)
	bus := &fakeBus{}
	gw := &fakeChatter{}
	h := buildHarness(t, s, gw, bus)

	convID := seedConversationWithMessages(t, s, owner)

	// other tries to read owner's conversation.
	rr := getConversationHistory(h, other, convID)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body: %s", rr.Code, rr.Body.String())
	}

	var prob map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &prob); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if code, _ := prob["code"].(string); code != "conversation_not_found" {
		t.Errorf("code = %q, want conversation_not_found", code)
	}
}

// TestGetConversationHistory_FullHistory verifies the full chronological history
// with correct shape (id, role, content, createdAt, abandoned).
func TestGetConversationHistory_FullHistory(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	gw := &fakeChatter{}
	h := buildHarness(t, s, gw, bus)

	convID := seedConversationWithMessages(t, s, p)

	rr := getConversationHistory(h, p, convID)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var resp map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// Check required top-level fields.
	for _, field := range []string{"id", "createdAt", "updatedAt", "messages"} {
		if _, ok := resp[field]; !ok {
			t.Errorf("missing field %q in response", field)
		}
	}

	var convID2 string
	if err := json.Unmarshal(resp["id"], &convID2); err != nil || convID2 != convID {
		t.Errorf("id = %q, want %q", convID2, convID)
	}

	var msgs []map[string]any
	if err := json.Unmarshal(resp["messages"], &msgs); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Errorf("len(messages) = %d, want 2", len(msgs))
	}
	if len(msgs) > 0 {
		// First message should be the user message.
		if role, _ := msgs[0]["role"].(string); role != "user" {
			t.Errorf("msgs[0].role = %q, want user", role)
		}
		if _, ok := msgs[0]["id"]; !ok {
			t.Error("msgs[0] missing id")
		}
		if _, ok := msgs[0]["createdAt"]; !ok {
			t.Error("msgs[0] missing createdAt")
		}
		if _, ok := msgs[0]["abandoned"]; !ok {
			t.Error("msgs[0] missing abandoned field")
		}
	}
}

// TestGetConversationHistory_ToolCallsShape verifies that tool_calls, toolCallId,
// and finishReason appear correctly in the response.
func TestGetConversationHistory_ToolCallsShape(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	gw := &fakeChatter{}
	h := buildHarness(t, s, gw, bus)

	convID := id.New()
	scope := store.UserScope(p)
	sq := s.ForUser(scope)
	if err := sq.UpsertConversation(context.Background(), convID); err != nil {
		t.Fatalf("UpsertConversation: %v", err)
	}

	// User message.
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             id.New(),
		ConversationID: convID,
		Role:           "user",
		Content:        "do the thing",
	}); err != nil {
		t.Fatalf("InsertMessage user: %v", err)
	}

	// Assistant message with tool_calls.
	toolCallsJSON := `[{"id":"call_abc","name":"my_tool","arguments":"{}"}]`
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             id.New(),
		ConversationID: convID,
		Role:           "assistant",
		Content:        "",
		ToolCalls:      toolCallsJSON,
		FinishReason:   "tool_use",
	}); err != nil {
		t.Fatalf("InsertMessage assistant: %v", err)
	}

	// Tool result message.
	if _, err := sq.InsertMessage(context.Background(), store.InsertMessageParams{
		ID:             id.New(),
		ConversationID: convID,
		Role:           "tool",
		Content:        `{"result":"done"}`,
		ToolCallID:     "call_abc",
	}); err != nil {
		t.Fatalf("InsertMessage tool: %v", err)
	}

	rr := getConversationHistory(h, p, convID)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]json.RawMessage
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(resp["messages"], &msgs); err != nil {
		t.Fatalf("decode messages: %v", err)
	}

	if len(msgs) != 3 {
		t.Fatalf("len(messages) = %d, want 3", len(msgs))
	}

	// Check assistant message has toolCalls and finishReason.
	assistantMsg := msgs[1]
	if _, ok := assistantMsg["toolCalls"]; !ok {
		t.Error("assistant message missing toolCalls field")
	}
	if _, ok := assistantMsg["finishReason"]; !ok {
		t.Error("assistant message missing finishReason field")
	}

	// Check tool message has toolCallId (not null).
	toolMsg := msgs[2]
	if _, ok := toolMsg["toolCallId"]; !ok {
		t.Error("tool message missing toolCallId field")
	}

	// toolCalls and finishReason should be absent on the user message (omitempty).
	userMsgRaw := msgs[0]
	if _, ok := userMsgRaw["toolCalls"]; ok {
		t.Error("user message should not have toolCalls field")
	}
	if _, ok := userMsgRaw["finishReason"]; ok {
		t.Error("user message should not have finishReason field")
	}
}

// ── Part 3: conversationId on every chat event ────────────────────────────────

// TestChatEvents_CompletedPayload_HasConversationID verifies that message.completed
// carries conversationId.
func TestChatEvents_CompletedPayload_HasConversationID(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	gw := &fakeChatter{chunks: []gateway.Chunk{{TextDelta: "hi"}, {Done: true}}}
	h := buildHarness(t, s, gw, bus)
	srv := buildServer(h)

	convID := id.New()
	body, _ := json.Marshal(map[string]string{"content": "hello"})
	req := makeAuthedRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", body, p)
	rr := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d", rr.Code)
	}

	evs := waitForCompleted(t, bus, 0, 3*time.Second)

	var found bool
	for _, e := range evs {
		if e.event.Type != "message.completed" {
			continue
		}
		payload, ok := e.event.Data.(harness.CompletedPayload)
		if !ok {
			t.Errorf("completed Data type = %T, want harness.CompletedPayload", e.event.Data)
			continue
		}
		found = true
		if payload.ConversationID == "" {
			t.Error("message.completed conversationId is empty")
		}
		if payload.ConversationID != convID {
			t.Errorf("message.completed conversationId = %q, want %q", payload.ConversationID, convID)
		}
	}
	if !found {
		t.Error("no message.completed event found")
	}
}

// TestChatEvents_TurnFailedPayload_HasConversationID verifies that turn.failed
// carries conversationId.
func TestChatEvents_TurnFailedPayload_HasConversationID(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	p := makeTestUser(t, s)
	bus := &fakeBus{}
	// Gateway that always fails with an infra error.
	gw := &fakeChatter{err: errInfraFail}
	h := buildHarness(t, s, gw, bus)
	srv := buildServer(h)

	convID := id.New()
	body, _ := json.Marshal(map[string]string{"content": "hello"})
	req := makeAuthedRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", body, p)
	rr := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d", rr.Code)
	}

	evs := waitForTurnFailed(t, bus, 3*time.Second)

	var found bool
	for _, e := range evs {
		if e.event.Type != "turn.failed" {
			continue
		}
		payload, ok := e.event.Data.(harness.TurnFailedPayload)
		if !ok {
			t.Errorf("turn.failed Data type = %T, want harness.TurnFailedPayload", e.event.Data)
			continue
		}
		found = true
		if payload.ConversationID == "" {
			t.Error("turn.failed conversationId is empty")
		}
		if payload.ConversationID != convID {
			t.Errorf("turn.failed conversationId = %q, want %q", payload.ConversationID, convID)
		}
	}
	if !found {
		t.Error("no turn.failed event found")
	}
}

// errInfraFail is a non-context-length infrastructure error for testing.
var errInfraFail = &infraError{msg: "infra failure"}

type infraError struct{ msg string }

func (e *infraError) Error() string { return e.msg }
