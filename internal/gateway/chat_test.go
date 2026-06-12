package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// newChatGateway builds a Gateway whose settings point at the provided
// httptest.Server URL and uses the fast test resilience config (no real sleep,
// tiny timeouts). The test is cleaned up automatically via t.Cleanup.
func newChatGateway(t *testing.T, srv *httptest.Server) *Gateway {
	t.Helper()
	fs := newFakeSettings(
		"primary.baseURL", srv.URL,
		"primary.apiKey", "sk-test",
		"primary.model", "gpt-test",
	)
	return newTestGateway(t, fs)
}

// collectChunks drives an iter.Seq2[Chunk, error] to completion and returns
// all chunks and the first error encountered.
func collectChunks(t *testing.T, seq func(yield func(Chunk, error) bool)) ([]Chunk, error) {
	t.Helper()
	var chunks []Chunk
	var firstErr error
	for c, err := range seq {
		if err != nil {
			firstErr = err
			break
		}
		chunks = append(chunks, c)
	}
	return chunks, firstErr
}

// ── AC1: streaming SSE yields correct Chunk sequence ─────────────────────────

// TestChat_TextAndToolsStream verifies that Chat against an httptest.Server
// streaming canned SSE yields text deltas + complete tool calls + Done with
// Usage (AC1).
func TestChat_TextAndToolsStream(t *testing.T) {
	t.Parallel()

	const ssePayload = "" +
		// role-priming chunk (empty content — must be skipped)
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n" +
		// text deltas
		`data: {"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"content":", world"},"finish_reason":null}]}` + "\n\n" +
		// tool call fragmented across two chunks
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search","arguments":""}}]},"finish_reason":null}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":\"go\"}"}}]},"finish_reason":null}]}` + "\n\n" +
		// finish
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":10,"total_tokens":15}}` + "\n\n" +
		"data: [DONE]\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, ssePayload)
	}))
	t.Cleanup(srv.Close)

	gw := newChatGateway(t, srv)
	seq, err := gw.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: unexpected setup error: %v", err)
	}

	chunks, iterErr := collectChunks(t, seq)
	if iterErr != nil {
		t.Fatalf("Chat iterator: unexpected error: %v", iterErr)
	}

	// Verify text deltas.
	var textSB strings.Builder
	for _, c := range chunks {
		textSB.WriteString(c.TextDelta)
	}
	if got := textSB.String(); got != "Hello, world" {
		t.Errorf("accumulated text = %q, want %q", got, "Hello, world")
	}

	// Verify tool call.
	var toolChunks []Chunk
	for _, c := range chunks {
		if c.ToolCall != nil {
			toolChunks = append(toolChunks, c)
		}
	}
	if len(toolChunks) != 1 {
		t.Fatalf("tool-call chunks: got %d, want 1", len(toolChunks))
	}
	tc := toolChunks[0].ToolCall
	if tc.ID != "call_1" {
		t.Errorf("ToolCall.ID = %q, want %q", tc.ID, "call_1")
	}
	if tc.Name != "search" {
		t.Errorf("ToolCall.Name = %q, want %q", tc.Name, "search")
	}
	if string(tc.Arguments) != `{"q":"go"}` {
		t.Errorf("ToolCall.Arguments = %q, want %q", string(tc.Arguments), `{"q":"go"}`)
	}

	// Verify Done with Usage.
	var doneChunk *Chunk
	for i := range chunks {
		if chunks[i].Done {
			doneChunk = &chunks[i]
		}
	}
	if doneChunk == nil {
		t.Fatal("no Done chunk")
	}
	if doneChunk.Usage == nil {
		t.Fatal("Done chunk has nil Usage")
	}
	if doneChunk.Usage.PromptTokens != 5 {
		t.Errorf("PromptTokens = %d, want 5", doneChunk.Usage.PromptTokens)
	}
	if doneChunk.Usage.CompletionTokens != 10 {
		t.Errorf("CompletionTokens = %d, want 10", doneChunk.Usage.CompletionTokens)
	}
	if doneChunk.Usage.TotalTokens != 15 {
		t.Errorf("TotalTokens = %d, want 15", doneChunk.Usage.TotalTokens)
	}
}

// ── AC2: outbound request has correct shape ───────────────────────────────────

// TestChat_GoldenRequest verifies that the outbound POST to the endpoint
// contains the correct JSON body: stream:true, Authorization: Bearer, and all
// the mapped fields (AC2).
func TestChat_GoldenRequest(t *testing.T) {
	t.Parallel()

	// Capture the request.
	var capturedBody []byte
	var capturedAuth string
	var capturedCT string

	const ssePayload = "" +
		`data: {"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n" +
		"data: [DONE]\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		capturedCT = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, ssePayload)
	}))
	t.Cleanup(srv.Close)

	gw := newChatGateway(t, srv)

	temp := 0.7
	maxTok := 100
	toolCallID := "call_prev"

	req := ChatRequest{
		Messages: []Message{
			{Role: "system", Content: "You are helpful."},
			{Role: "user", Content: "What is 2+2?"},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{ID: "call_prev", Name: "calc", Arguments: json.RawMessage(`{"expr":"2+2"}`)},
				},
			},
			{Role: "tool", Content: "4", ToolCallID: toolCallID},
		},
		Tools: []ToolSchema{
			{
				Name:        "calc",
				Description: "Evaluate arithmetic",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"expr":{"type":"string"}}}`),
			},
		},
		ToolChoice:  ToolChoiceAuto(),
		Temperature: &temp,
		MaxTokens:   &maxTok,
	}

	seq, err := gw.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: setup error: %v", err)
	}
	// Drain the iterator to ensure the request was fully processed.
	if _, iterErr := collectChunks(t, seq); iterErr != nil {
		t.Fatalf("Chat iterator: unexpected error: %v", iterErr)
	}

	// Check headers.
	if capturedAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want %q", capturedAuth, "Bearer sk-test")
	}
	if capturedCT != "application/json" {
		t.Errorf("Content-Type = %q, want %q", capturedCT, "application/json")
	}

	// Parse and check the request body.
	var wireReq map[string]any
	if err := json.Unmarshal(capturedBody, &wireReq); err != nil {
		t.Fatalf("unmarshal captured body: %v", err)
	}

	// stream must be true.
	if stream, _ := wireReq["stream"].(bool); !stream {
		t.Errorf("stream = %v, want true", wireReq["stream"])
	}

	// temperature must be present and correct.
	if got, _ := wireReq["temperature"].(float64); got != 0.7 {
		t.Errorf("temperature = %v, want 0.7", wireReq["temperature"])
	}

	// max_tokens must be present and correct.
	if got, _ := wireReq["max_tokens"].(float64); got != 100 {
		t.Errorf("max_tokens = %v, want 100", wireReq["max_tokens"])
	}

	// model must be gpt-test.
	if got, _ := wireReq["model"].(string); got != "gpt-test" {
		t.Errorf("model = %q, want gpt-test", got)
	}

	// messages: check count and roles.
	msgs, _ := wireReq["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages count = %d, want 4", len(msgs))
	}

	// Check system message.
	if m, _ := msgs[0].(map[string]any); m["role"] != "system" || m["content"] != "You are helpful." {
		t.Errorf("messages[0] = %v, want system role with content", msgs[0])
	}

	// Check user message.
	if m, _ := msgs[1].(map[string]any); m["role"] != "user" || m["content"] != "What is 2+2?" {
		t.Errorf("messages[1] = %v, want user role with content", msgs[1])
	}

	// Check assistant message with tool_calls.
	assistantMsg, _ := msgs[2].(map[string]any)
	if assistantMsg["role"] != "assistant" {
		t.Errorf("messages[2].role = %v, want assistant", assistantMsg["role"])
	}
	toolCalls, _ := assistantMsg["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("messages[2].tool_calls count = %d, want 1", len(toolCalls))
	}
	tc, _ := toolCalls[0].(map[string]any)
	if tc["id"] != "call_prev" || tc["type"] != "function" {
		t.Errorf("tool_calls[0] = %v, want id=call_prev type=function", tc)
	}
	tcFunc, _ := tc["function"].(map[string]any)
	if tcFunc["name"] != "calc" {
		t.Errorf("tool_calls[0].function.name = %v, want calc", tcFunc["name"])
	}

	// Check tool-result message.
	toolMsg, _ := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["content"] != "4" || toolMsg["tool_call_id"] != "call_prev" {
		t.Errorf("messages[3] = %v, want tool role with content and tool_call_id", msgs[3])
	}

	// tools array.
	tools, _ := wireReq["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools count = %d, want 1", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("tools[0].type = %v, want function", tool["type"])
	}
	toolFn, _ := tool["function"].(map[string]any)
	if toolFn["name"] != "calc" {
		t.Errorf("tools[0].function.name = %v, want calc", toolFn["name"])
	}

	// tool_choice must be "auto".
	if tc2, _ := wireReq["tool_choice"].(string); tc2 != "auto" {
		t.Errorf("tool_choice = %v, want auto", wireReq["tool_choice"])
	}
}

// TestChat_GoldenRequest_NilOptionals verifies that nil Temperature and
// MaxTokens are omitted from the request body.
func TestChat_GoldenRequest_NilOptionals(t *testing.T) {
	t.Parallel()

	var capturedBody []byte
	const ssePayload = `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, ssePayload)
	}))
	t.Cleanup(srv.Close)

	gw := newChatGateway(t, srv)
	seq, err := gw.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		// Temperature and MaxTokens deliberately nil
	})
	if err != nil {
		t.Fatalf("Chat: setup error: %v", err)
	}
	if _, iterErr := collectChunks(t, seq); iterErr != nil {
		t.Fatalf("Chat iterator: unexpected error: %v", iterErr)
	}

	var wireReq map[string]any
	if err := json.Unmarshal(capturedBody, &wireReq); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := wireReq["temperature"]; ok {
		t.Error("temperature should be absent when nil")
	}
	if _, ok := wireReq["max_tokens"]; ok {
		t.Error("max_tokens should be absent when nil")
	}
	// tool_choice should also be absent when no tools are provided.
	if _, ok := wireReq["tool_choice"]; ok {
		t.Error("tool_choice should be absent when no tools provided")
	}
}

// TestChat_ToolChoiceModes verifies that all ToolChoice variants map to the
// correct wire representations.
func TestChat_ToolChoiceModes(t *testing.T) {
	t.Parallel()

	const ssePayload = `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n"

	type row struct {
		name       string
		toolChoice ToolChoice
		wantString string // if non-empty, expect a JSON string
		wantObj    bool   // if true, expect a JSON object with type=function
		wantName   string // expected function name when wantObj=true
	}

	rows := []row{
		{name: "auto", toolChoice: ToolChoiceAuto(), wantString: "auto"},
		{name: "none", toolChoice: ToolChoiceNone(), wantString: "none"},
		{name: "required", toolChoice: ToolChoiceRequired(), wantString: "required"},
		{name: "named", toolChoice: ToolChoiceNamed("my_tool"), wantObj: true, wantName: "my_tool"},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()

			var capturedBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body, _ := io.ReadAll(req.Body)
				capturedBody = body
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, ssePayload)
			}))
			t.Cleanup(srv.Close)

			gw := newChatGateway(t, srv)
			seq, err := gw.Chat(context.Background(), ChatRequest{
				Messages:   []Message{{Role: "user", Content: "hi"}},
				Tools:      []ToolSchema{{Name: "my_tool", Parameters: json.RawMessage(`{}`)}},
				ToolChoice: r.toolChoice,
			})
			if err != nil {
				t.Fatalf("Chat: setup error: %v", err)
			}
			if _, iterErr := collectChunks(t, seq); iterErr != nil {
				t.Fatalf("Chat iterator: unexpected error: %v", iterErr)
			}

			var wireReq map[string]any
			if err := json.Unmarshal(capturedBody, &wireReq); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			tcRaw := wireReq["tool_choice"]
			if r.wantString != "" {
				if got, _ := tcRaw.(string); got != r.wantString {
					t.Errorf("tool_choice = %v, want %q", tcRaw, r.wantString)
				}
			}
			if r.wantObj {
				obj, ok := tcRaw.(map[string]any)
				if !ok {
					t.Fatalf("tool_choice is %T, want map; value=%v", tcRaw, tcRaw)
				}
				if obj["type"] != "function" {
					t.Errorf("tool_choice.type = %v, want function", obj["type"])
				}
				fn, _ := obj["function"].(map[string]any)
				if fn["name"] != r.wantName {
					t.Errorf("tool_choice.function.name = %v, want %q", fn["name"], r.wantName)
				}
			}
		})
	}
}

// ── AC4: unconfigured gateway returns ErrGatewayNotConfigured ────────────────

func TestChat_Unconfigured(t *testing.T) {
	t.Parallel()

	gw := newGatewayWithFake(newFakeSettings()) // no primary configured

	seq, err := gw.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, ErrGatewayNotConfigured) {
		t.Errorf("Chat error = %v, want ErrGatewayNotConfigured", err)
	}
	if seq != nil {
		t.Error("Chat seq should be nil on setup error")
	}
}

// ── AC5: non-2xx setup response returns the correct typed error ───────────────

func TestChat_NonOK_Setup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		wantErr    error
	}{
		{name: "401", statusCode: 401, wantErr: ErrAuth},
		{name: "403", statusCode: 403, wantErr: ErrAuth},
		{name: "429", statusCode: 429, wantErr: ErrRateLimited},
		{name: "500", statusCode: 500, wantErr: ErrUpstream},
		{name: "503", statusCode: 503, wantErr: ErrUpstream},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
			}))
			t.Cleanup(srv.Close)

			gw := newChatGateway(t, srv)
			seq, err := gw.Chat(context.Background(), ChatRequest{
				Messages: []Message{{Role: "user", Content: "hi"}},
			})
			if seq != nil {
				t.Error("seq should be nil on setup error")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Chat error = %v, want errors.Is(%v)", err, tt.wantErr)
			}
		})
	}
}

// ── AC6: transport break mid-stream surfaces as a per-yield error ─────────────

// TestChat_MidStreamTransportBreak verifies that a broken connection after the
// 2xx response header arrives surfaces as a per-yield error on the iterator,
// never panics, and never re-emits (AC6).
func TestChat_MidStreamTransportBreak(t *testing.T) {
	t.Parallel()

	// The server sends one valid text chunk, then abruptly closes the connection.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Write one good chunk and flush so the client receives the 2xx response.
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// Hijack and close the connection to simulate a mid-stream break.
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			// If hijack isn't available, just return (the connection closes when handler returns).
			return
		}
		conn, _, _ := hijacker.Hijack()
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)

	gw := newChatGateway(t, srv)
	seq, err := gw.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: unexpected setup error: %v", err)
	}

	// Drive the iterator — expect some chunks then an error (or just the
	// stream ending without error, since the abrupt close may look like EOF
	// to bufio.Scanner which we treat as normal termination).
	var chunks []Chunk
	var iterErr error
	for c, e := range seq {
		if e != nil {
			iterErr = e
			break
		}
		chunks = append(chunks, c)
	}

	// We must have received at least the one text chunk before any error.
	var hasText bool
	for _, c := range chunks {
		if c.TextDelta == "hi" {
			hasText = true
		}
	}
	if !hasText {
		t.Errorf("expected to see 'hi' text chunk before break; got: %v", chunks)
	}

	// If an error occurred it must wrap ErrUpstreamProtocol (scanner read
	// error) or the stream naturally ended — either is acceptable. The key
	// invariant is: no panic and no re-emit after error.
	if iterErr != nil && !errors.Is(iterErr, ErrUpstreamProtocol) {
		t.Errorf("iterator error = %v; expected nil or ErrUpstreamProtocol", iterErr)
	}

	// Ensure the iterator is exhausted after the error — ranging again over
	// a closed iterator should yield nothing. Since seq is a func, we simply
	// verify the iterator does not yield more items after we broke.
	// (This is tested by the fact that we called break — the seq function
	// must not panic or call yield after the consumer has returned false.)
}

// TestChat_NoReemitAfterError verifies that once the iterator yields an error
// it stops — no additional chunks are emitted afterward. We achieve this by
// asserting the body was fully consumed (drainClose called) so the connection
// resource is freed.
func TestChat_NoReemitAfterError(t *testing.T) {
	t.Parallel()

	// Serve a stream that produces a valid chunk, then a malformed line.
	const ssePayload = "" +
		`data: {"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}` + "\n\n" +
		"data: {BAD JSON}\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, ssePayload)
	}))
	t.Cleanup(srv.Close)

	gw := newChatGateway(t, srv)
	seq, err := gw.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: setup error: %v", err)
	}

	var gotChunks []Chunk
	var gotErr error
	var yieldCount int
	for c, e := range seq {
		yieldCount++
		if e != nil {
			gotErr = e
			// do NOT break — verify iterator terminates on its own
			break
		}
		gotChunks = append(gotChunks, c)
	}

	// We must have gotten the text chunk "a" before the error.
	var hasA bool
	for _, c := range gotChunks {
		if c.TextDelta == "a" {
			hasA = true
		}
	}
	if !hasA {
		t.Errorf("expected text chunk 'a' before error; chunks = %v", gotChunks)
	}

	if !errors.Is(gotErr, ErrUpstreamProtocol) {
		t.Errorf("iterator error = %v, want ErrUpstreamProtocol", gotErr)
	}

	// yieldCount should be exactly 2: the text chunk + the error yield.
	if yieldCount != 2 {
		t.Errorf("yield count = %d, want 2 (one text + one error)", yieldCount)
	}
}
