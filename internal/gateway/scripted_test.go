//go:build e2e

package gateway_test

// scripted_test.go — unit tests for ScriptedChatter (e2e build tag only).
//
// Run with: go test -tags e2e ./internal/gateway/ -race

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/gateway"
)

// joinText concatenates the TextDelta fields of all chunks into a string.
func joinText(chunks []gateway.Chunk) string {
	var sb strings.Builder
	for _, c := range chunks {
		sb.WriteString(c.TextDelta)
	}
	return sb.String()
}

// ── helpers ───────────────────────────────────────────────────────────────────

// mustNewScripted constructs a ScriptedChatter from a JSON fixture string and
// fails the test if construction fails.
func mustNewScripted(t *testing.T, fixtureJSON string) *gateway.ScriptedChatter {
	t.Helper()
	sc, err := gateway.NewScriptedChatterFromJSON([]byte(fixtureJSON))
	if err != nil {
		t.Fatalf("NewScriptedChatterFromJSON: %v", err)
	}
	return sc
}

// collectChunks consumes the iterator from Chat and returns all chunks + any error.
func collectChunks(t *testing.T, sc *gateway.ScriptedChatter, req gateway.ChatRequest) ([]gateway.Chunk, error) {
	t.Helper()
	seq, err := sc.Chat(context.Background(), req)
	if err != nil {
		return nil, err
	}
	var chunks []gateway.Chunk
	for chunk, chunkErr := range seq {
		if chunkErr != nil {
			return chunks, chunkErr
		}
		chunks = append(chunks, chunk)
	}
	return chunks, nil
}

// buildRequest constructs a ChatRequest with the given messages.
func buildRequest(msgs ...gateway.Message) gateway.ChatRequest {
	return gateway.ChatRequest{Messages: msgs}
}

// userMsg returns a user-role Message with the given content.
func userMsg(content string) gateway.Message {
	return gateway.Message{Role: "user", Content: content}
}

// assistantMsg returns an assistant-role Message with optional tool calls.
func assistantMsg(content string, calls ...gateway.ToolCall) gateway.Message {
	m := gateway.Message{Role: "assistant", Content: content}
	if len(calls) > 0 {
		m.ToolCalls = calls
	}
	return m
}

// toolMsg returns a tool-role Message (a tool result).
func toolMsg(callID, content string) gateway.Message {
	return gateway.Message{Role: "tool", ToolCallID: callID, Content: content}
}

// systemMsg returns a system-role Message.
func systemMsg(content string) gateway.Message {
	return gateway.Message{Role: "system", Content: content}
}

// ── 1. Script JSON parsing ────────────────────────────────────────────────────

func TestScriptedChatter_Parsing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		json    string
		wantErr bool
	}{
		{
			name: "valid minimal fixture",
			json: `{
				"rules": [
					{
						"match": {"contains": "hello"},
						"rounds": [
							{"chunks": [{"textDelta": "Hi there!"}, {"done": true}]}
						]
					}
				]
			}`,
			wantErr: false,
		},
		{
			name: "valid with prefix match",
			json: `{
				"rules": [
					{
						"match": {"prefix": "create reminder"},
						"rounds": [
							{
								"chunks": [
									{"toolCall": {"name": "create_reminder", "arguments": {"title": "test", "dueAt": "2026-06-15T09:00:00Z"}}, "delayMs": 2},
									{"done": true}
								]
							}
						]
					}
				]
			}`,
			wantErr: false,
		},
		{
			name: "valid multi-round fixture",
			json: `{
				"rules": [
					{
						"match": {"contains": "delete"},
						"rounds": [
							{"chunks": [{"toolCall": {"name": "delete_reminder", "arguments": {"id": "01JXYZ"}}}, {"done": true}]},
							{"chunks": [{"textDelta": "Deleted."}, {"done": true}]}
						]
					}
				]
			}`,
			wantErr: false,
		},
		{
			name:    "malformed JSON",
			json:    `{not valid json`,
			wantErr: true,
		},
		{
			name: "missing rules key is okay (empty rules)",
			json: `{}`,
			// No error: empty rules list is valid; a no-match default fires.
			wantErr: false,
		},
		{
			name: "match with both contains and prefix is accepted",
			json: `{
				"rules": [
					{
						"match": {"contains": "foo", "prefix": "bar"},
						"rounds": [
							{"chunks": [{"done": true}]}
						]
					}
				]
			}`,
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := gateway.NewScriptedChatterFromJSON([]byte(tc.json))
			if (err != nil) != tc.wantErr {
				t.Errorf("NewScriptedChatterFromJSON err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// ── 2. Message-keyed rule selection ──────────────────────────────────────────

func TestScriptedChatter_RuleSelection(t *testing.T) {
	t.Parallel()

	fixture := `{
		"rules": [
			{
				"match": {"contains": "create"},
				"rounds": [{"chunks": [{"textDelta": "create-matched"}, {"done": true}]}]
			},
			{
				"match": {"prefix": "delete"},
				"rounds": [{"chunks": [{"textDelta": "delete-matched"}, {"done": true}]}]
			},
			{
				"match": {"contains": "list"},
				"rounds": [{"chunks": [{"textDelta": "list-matched"}, {"done": true}]}]
			}
		]
	}`
	sc := mustNewScripted(t, fixture)

	tests := []struct {
		name        string
		userContent string
		wantText    string
	}{
		{
			name:        "contains match — create",
			userContent: "please create a reminder for me",
			wantText:    "create-matched",
		},
		{
			name:        "prefix match — delete at start",
			userContent: "delete the reminder",
			wantText:    "delete-matched",
		},
		{
			name:        "contains match — list anywhere",
			userContent: "can you list my reminders?",
			wantText:    "list-matched",
		},
		{
			name:        "first matching rule wins",
			userContent: "create list something", // matches "create" first
			wantText:    "create-matched",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := buildRequest(systemMsg("sys"), userMsg(tc.userContent))
			chunks, err := collectChunks(t, sc, req)
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			text := joinText(chunks)
			if !strings.Contains(text, tc.wantText) {
				t.Errorf("text = %q, want to contain %q", text, tc.wantText)
			}
		})
	}
}

func TestScriptedChatter_CaseInsensitiveMatch(t *testing.T) {
	t.Parallel()

	fixture := `{
		"rules": [
			{
				"match": {"contains": "CREATE"},
				"rounds": [{"chunks": [{"textDelta": "matched"}, {"done": true}]}]
			}
		]
	}`
	sc := mustNewScripted(t, fixture)

	// lowercase user message should match UPPER-CASE pattern.
	req := buildRequest(userMsg("please create a reminder"))
	chunks, err := collectChunks(t, sc, req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	text := joinText(chunks)
	if !strings.Contains(text, "matched") {
		t.Errorf("expected case-insensitive match, got text = %q", text)
	}
}

// ── 3. Round-index computation ────────────────────────────────────────────────

// TestScriptedChatter_RoundIndex verifies that the round index is correctly
// derived from the ChatRequest history: round 0 on the first call (only user
// message present), round 1 on the second call (assistant+tool messages follow
// the user message), etc.
func TestScriptedChatter_RoundIndex(t *testing.T) {
	t.Parallel()

	// Two-round fixture: round 0 emits a tool call; round 1 emits completion text.
	fixture := `{
		"rules": [
			{
				"match": {"contains": "delete reminder"},
				"rounds": [
					{
						"chunks": [
							{"toolCall": {"name": "delete_reminder", "arguments": {"id": "r1"}}},
							{"done": true}
						]
					},
					{
						"chunks": [
							{"textDelta": "Reminder deleted."},
							{"done": true}
						]
					}
				]
			}
		]
	}`
	sc := mustNewScripted(t, fixture)

	t.Run("round 0 emits tool call", func(t *testing.T) {
		t.Parallel()
		// History: system + user only → round 0.
		req := buildRequest(
			systemMsg("You are an assistant."),
			userMsg("delete reminder r1"),
		)
		chunks, err := collectChunks(t, sc, req)
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}

		var gotToolCall bool
		var gotDone bool
		for _, c := range chunks {
			if c.ToolCall != nil {
				gotToolCall = true
				if c.ToolCall.Name != "delete_reminder" {
					t.Errorf("ToolCall.Name = %q, want delete_reminder", c.ToolCall.Name)
				}
			}
			if c.Done {
				gotDone = true
			}
		}
		if !gotToolCall {
			t.Error("round 0: expected ToolCall chunk, none found")
		}
		if !gotDone {
			t.Error("round 0: expected Done chunk, none found")
		}
	})

	t.Run("round 1 emits completion text after tool result in history", func(t *testing.T) {
		t.Parallel()
		// History: system + user + assistant-with-toolcall + tool-result → round 1.
		req := buildRequest(
			systemMsg("You are an assistant."),
			userMsg("delete reminder r1"),
			assistantMsg("", gateway.ToolCall{ID: "call1", Name: "delete_reminder", Arguments: json.RawMessage(`{"id":"r1"}`)}),
			toolMsg("call1", `{"deleted":true}`),
		)
		chunks, err := collectChunks(t, sc, req)
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}

		text := joinText(chunks)
		var gotDone bool
		for _, c := range chunks {
			if c.Done {
				gotDone = true
			}
		}
		if !strings.Contains(text, "Reminder deleted.") {
			t.Errorf("round 1: text = %q, want to contain %q", text, "Reminder deleted.")
		}
		if !gotDone {
			t.Error("round 1: expected Done chunk, none found")
		}
	})
}

// ── 4. Chunk ordering and emission ───────────────────────────────────────────

func TestScriptedChatter_ChunkOrdering(t *testing.T) {
	t.Parallel()

	fixture := `{
		"rules": [
			{
				"match": {"contains": "order test"},
				"rounds": [
					{
						"chunks": [
							{"textDelta": "first"},
							{"textDelta": "second"},
							{"textDelta": "third"},
							{"done": true}
						]
					}
				]
			}
		]
	}`
	sc := mustNewScripted(t, fixture)

	req := buildRequest(userMsg("order test"))
	seq, err := sc.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat setup error: %v", err)
	}

	var got []string
	doneIdx := -1
	i := 0
	for chunk, chunkErr := range seq {
		if chunkErr != nil {
			t.Fatalf("chunk error: %v", chunkErr)
		}
		if chunk.TextDelta != "" {
			got = append(got, chunk.TextDelta)
		}
		if chunk.Done {
			doneIdx = i
		}
		i++
	}

	want := []string{"first", "second", "third"}
	if len(got) != len(want) {
		t.Fatalf("chunk count = %d, want %d; got %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("chunk[%d] = %q, want %q", i, got[i], w)
		}
	}
	if doneIdx < 0 {
		t.Error("Done chunk not emitted")
	}
}

// ── 5. delayMs honoured ───────────────────────────────────────────────────────

func TestScriptedChatter_DelayHonoured(t *testing.T) {
	t.Parallel()

	// 3ms delay per chunk; we emit 2 delayed chunks.
	fixture := `{
		"rules": [
			{
				"match": {"contains": "delay test"},
				"rounds": [
					{
						"chunks": [
							{"textDelta": "a", "delayMs": 3},
							{"textDelta": "b", "delayMs": 3},
							{"done": true}
						]
					}
				]
			}
		]
	}`
	sc := mustNewScripted(t, fixture)

	req := buildRequest(userMsg("delay test"))
	start := time.Now()
	chunks, err := collectChunks(t, sc, req)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
	// Two 3ms delays = at least 6ms total. Allow generous tolerance.
	const minElapsed = 5 * time.Millisecond
	if elapsed < minElapsed {
		t.Errorf("elapsed = %v, want >= %v (delays not honoured)", elapsed, minElapsed)
	}
}

// ── 6. ctx cancellation aborts mid-script ─────────────────────────────────────

func TestScriptedChatter_CtxCancellation(t *testing.T) {
	t.Parallel()

	// Long delays so we can cancel before the sequence ends.
	fixture := `{
		"rules": [
			{
				"match": {"contains": "cancel test"},
				"rounds": [
					{
						"chunks": [
							{"textDelta": "before-cancel", "delayMs": 50},
							{"textDelta": "after-cancel-should-not-appear", "delayMs": 50},
							{"done": true}
						]
					}
				]
			}
		]
	}`
	sc := mustNewScripted(t, fixture)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // ensure the cancel is always called (lostcancel guard)
	req := buildRequest(userMsg("cancel test"))

	seq, err := sc.Chat(ctx, req)
	if err != nil {
		t.Fatalf("Chat setup: %v", err)
	}

	var chunks []gateway.Chunk
	var gotErr error
	for chunk, chunkErr := range seq {
		if chunkErr != nil {
			gotErr = chunkErr
			break
		}
		chunks = append(chunks, chunk)
		// Cancel after receiving the first chunk.
		if len(chunks) == 1 {
			cancel()
		}
	}

	// Either the iterator stopped yielding (break) or returned a context error.
	// The implementation should return ctx.Err() when the context is cancelled.
	// We allow either termination mode: what matters is that "after-cancel" text
	// did not appear.
	for _, c := range chunks {
		if strings.Contains(c.TextDelta, "after-cancel") {
			t.Error("chunk emitted after context cancellation")
		}
	}
	// gotErr may be nil (iterator stopped early) or context.Canceled.
	if gotErr != nil && gotErr != context.Canceled {
		t.Errorf("unexpected error: %v", gotErr)
	}
}

// ── 7. No-match default ───────────────────────────────────────────────────────

func TestScriptedChatter_NoMatchDefault(t *testing.T) {
	t.Parallel()

	fixture := `{
		"rules": [
			{
				"match": {"contains": "specific phrase"},
				"rounds": [{"chunks": [{"textDelta": "specific reply"}, {"done": true}]}]
			}
		]
	}`
	sc := mustNewScripted(t, fixture)

	// Message that does NOT match any rule.
	req := buildRequest(userMsg("something completely different"))
	chunks, err := collectChunks(t, sc, req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// Should emit a short text + Done (the safe default), not hang.
	var gotDone bool
	for _, c := range chunks {
		if c.Done {
			gotDone = true
		}
	}
	if !gotDone {
		t.Error("no-match default: expected Done chunk, none found")
	}
	// Should have at least some text delta.
	if joinText(chunks) == "" {
		t.Error("no-match default: expected non-empty text, got empty")
	}
}

// ── 8. Out-of-range round default ─────────────────────────────────────────────

func TestScriptedChatter_OutOfRangeRoundDefault(t *testing.T) {
	t.Parallel()

	// Only one round defined; simulate calling beyond it.
	fixture := `{
		"rules": [
			{
				"match": {"contains": "test"},
				"rounds": [
					{"chunks": [{"textDelta": "round0"}, {"done": true}]}
				]
			}
		]
	}`
	sc := mustNewScripted(t, fixture)

	// Build a request whose history implies round index 1
	// (user message + 1 assistant + 1 tool = round 1), but only round 0 exists.
	req := buildRequest(
		userMsg("test"),
		assistantMsg("", gateway.ToolCall{ID: "c1", Name: "some_tool", Arguments: json.RawMessage(`{}`)}),
		toolMsg("c1", `{"ok":true}`),
	)
	chunks, err := collectChunks(t, sc, req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// Should emit safe default (text + Done), not panic or return error.
	var gotDone bool
	for _, c := range chunks {
		if c.Done {
			gotDone = true
		}
	}
	if !gotDone {
		t.Error("out-of-range round: expected Done chunk in default response")
	}
}

// ── 9. ToolCall chunk carries correct fields ──────────────────────────────────

func TestScriptedChatter_ToolCallChunkFields(t *testing.T) {
	t.Parallel()

	fixture := `{
		"rules": [
			{
				"match": {"contains": "create reminder"},
				"rounds": [
					{
						"chunks": [
							{
								"toolCall": {
									"name": "create_reminder",
									"arguments": {"title": "Buy milk", "dueAt": "2026-06-15T09:00:00Z"}
								}
							},
							{"done": true}
						]
					}
				]
			}
		]
	}`
	sc := mustNewScripted(t, fixture)

	req := buildRequest(userMsg("create reminder: Buy milk tomorrow at 9am"))
	chunks, err := collectChunks(t, sc, req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	var toolChunk *gateway.Chunk
	for i := range chunks {
		if chunks[i].ToolCall != nil {
			c := chunks[i]
			toolChunk = &c
			break
		}
	}
	if toolChunk == nil {
		t.Fatal("no ToolCall chunk found")
	}
	if toolChunk.ToolCall.Name != "create_reminder" {
		t.Errorf("ToolCall.Name = %q, want create_reminder", toolChunk.ToolCall.Name)
	}
	// ID should be auto-generated (non-empty).
	if toolChunk.ToolCall.ID == "" {
		t.Error("ToolCall.ID is empty; should be auto-generated")
	}
	// Arguments should be valid JSON containing the fixture's arguments.
	var args map[string]string
	if err := json.Unmarshal(toolChunk.ToolCall.Arguments, &args); err != nil {
		t.Errorf("ToolCall.Arguments not valid JSON: %v", err)
	}
	if args["title"] != "Buy milk" {
		t.Errorf("ToolCall.Arguments[title] = %q, want %q", args["title"], "Buy milk")
	}
}

// ── 10. Latest-user-message keying ───────────────────────────────────────────

// TestScriptedChatter_LatestUserMessageKeying verifies that the scripted chatter
// keys off the LATEST user message in the request, not the first. This is the
// multi-turn case: prior exchanges shouldn't affect the rule selection.
func TestScriptedChatter_LatestUserMessageKeying(t *testing.T) {
	t.Parallel()

	fixture := `{
		"rules": [
			{
				"match": {"contains": "first question"},
				"rounds": [{"chunks": [{"textDelta": "first-reply"}, {"done": true}]}]
			},
			{
				"match": {"contains": "second question"},
				"rounds": [{"chunks": [{"textDelta": "second-reply"}, {"done": true}]}]
			}
		]
	}`
	sc := mustNewScripted(t, fixture)

	// History has two user turns; the latest is "second question".
	req := buildRequest(
		systemMsg("sys"),
		userMsg("first question"),
		assistantMsg("first-reply"),
		userMsg("second question"),
	)
	chunks, err := collectChunks(t, sc, req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	text := joinText(chunks)
	if !strings.Contains(text, "second-reply") {
		t.Errorf("text = %q, want to contain %q", text, "second-reply")
	}
}
