package gateway

// Tests for [Gateway.Probe].
//
// All tests use httptest.Server with a handler that routes on path:
//   - GET  /v1/models              → JSON model list
//   - POST /v1/chat/completions    → SSE stream
//
// The "unreachable" case uses a server that is closed before the probe runs. For "bad key", the handler returns
// 401 when the bearer token does not match an expected sentinel.
//
// All tests run under -race.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// ── SSE fixtures ──────────────────────────────────────────────────────────────

// toolCallSSE is a minimal SSE stream that yields one tool call and a Done chunk.
const toolCallSSE = "" +
	`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_probe","type":"function","function":{"name":"_probe","arguments":""}}]},"finish_reason":null}]}` + "\n\n" +
	`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":null}]}` + "\n\n" +
	`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":5,"total_tokens":10}}` + "\n\n" +
	"data: [DONE]\n"

// plainTextSSE is a minimal SSE stream that yields only text (no tool call).
const plainTextSSE = "" +
	`data: {"choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}` + "\n\n" +
	`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}` + "\n\n" +
	"data: [DONE]\n"

// ── handler helpers ───────────────────────────────────────────────────────────

// modelsJSON encodes a /v1/models response listing the given model IDs.
func modelsJSON(ids ...string) string {
	type entry struct {
		ID string `json:"id"`
	}
	type resp struct {
		Data []entry `json:"data"`
	}
	r := resp{Data: make([]entry, len(ids))}
	for i, id := range ids {
		r.Data[i] = entry{ID: id}
	}
	b, err := json.Marshal(r)
	if err != nil {
		panic(fmt.Sprintf("modelsJSON: marshal: %v", err))
	}
	return string(b)
}

// writeSSEProbe writes an SSE response to w with the correct headers.
func writeSSEProbe(w http.ResponseWriter, payload string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, payload)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// newProbeServer constructs an httptest.Server whose handler:
//   - On GET /v1/models: checks the bearer token against wantKey (returns 401
//     if mismatched); returns modelsBody as JSON.
//   - On POST /v1/chat/completions: streams chatSSE as an SSE response.
//
// If wantKey is empty, no bearer validation is performed. If customHandler is non-nil it replaces the default
// routing entirely, allowing individual test cases to inject arbitrary per-path behaviour.
func newProbeServer(t *testing.T, wantKey, modelsBody, chatSSE string, customHandler http.HandlerFunc) *httptest.Server {
	t.Helper()

	var h http.HandlerFunc
	if customHandler != nil {
		h = customHandler
	} else {
		h = func(w http.ResponseWriter, r *http.Request) {
			// Bearer token check (both paths use the same key).
			if wantKey != "" {
				auth := r.Header.Get("Authorization")
				if auth != "Bearer "+wantKey {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
			}

			switch r.URL.Path {
			case "/v1/models":
				if r.Method != http.MethodGet {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, modelsBody)

			case "/v1/chat/completions":
				if r.Method != http.MethodPost {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				writeSSEProbe(w, chatSSE)

			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}
	}

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// newProbeGateway builds a Gateway pointed at srv with the given API key and model name.
func newProbeGateway(t *testing.T, baseURL, apiKey, model string) *Gateway {
	t.Helper()
	fs := newFakeSettings(
		"primary.baseURL", baseURL,
		"primary.apiKey", apiKey,
		"primary.model", model,
	)
	return newGatewayWithFake(fs)
}

// ── Table-driven probe tests ──────────────────────────────────────────────────

func TestProbe_TableDriven(t *testing.T) {
	t.Parallel()

	const (
		goodKey   = "sk-good"
		goodModel = "test-model"
	)

	tests := []struct {
		name string

		// server setup — empty chatSSE means no chat endpoint needed. customHandler, when non-nil, replaces the
		// default routing entirely.
		serverKey     string           // key the server expects (empty = no auth check)
		modelsBody    string           // JSON to return from /v1/models
		chatSSE       string           // SSE payload from /v1/chat/completions
		unreachable   bool             // close the server before probing
		customHandler http.HandlerFunc // replaces default handler when set

		// gateway setup
		apiKey string // key the gateway sends
		model  string // model the gateway is configured for

		role Role

		// expected result
		wantReachable   bool
		wantModelServed bool
		wantToolCalling bool
		wantStreaming   bool
		wantErr         bool   // any non-nil Err
		wantErrIs       error  // specific sentinel (optional)
		wantErrContains string // substring in Err.Error() (optional)
	}{
		// AC1: full success — all four bools true.
		{
			name:            "full_success_tool_call_returned",
			serverKey:       goodKey,
			modelsBody:      modelsJSON(goodModel),
			chatSSE:         toolCallSSE,
			apiKey:          goodKey,
			model:           goodModel,
			role:            RoleChat,
			wantReachable:   true,
			wantModelServed: true,
			wantToolCalling: true,
			wantStreaming:   true,
		},
		// AC4: endpoint returns only plain text — ToolCalling=false.
		{
			name:            "plain_text_no_tool_call",
			serverKey:       goodKey,
			modelsBody:      modelsJSON(goodModel),
			chatSSE:         plainTextSSE,
			apiKey:          goodKey,
			model:           goodModel,
			role:            RoleChat,
			wantReachable:   true,
			wantModelServed: true,
			wantToolCalling: false,
			wantStreaming:   true,
		},
		// AC3: model not in the list — ModelServed=false, no step 2.
		{
			name:            "model_absent_from_list",
			serverKey:       goodKey,
			modelsBody:      modelsJSON("other-model"),
			apiKey:          goodKey,
			model:           goodModel,
			role:            RoleChat,
			wantReachable:   true,
			wantModelServed: false,
			wantToolCalling: false,
			wantStreaming:   false,
		},
		// AC2 (auth): bad API key → 401 → auth error in Err.
		{
			name:          "bad_key_401",
			serverKey:     goodKey,
			modelsBody:    modelsJSON(goodModel),
			apiKey:        "sk-wrong",
			model:         goodModel,
			role:          RoleChat,
			wantReachable: true, // endpoint responded (with 401)
			wantErr:       true,
			wantErrIs:     ErrAuth,
		},
		// AC2 (unreachable): closed server → dial error.
		{
			name:          "unreachable_endpoint",
			unreachable:   true,
			serverKey:     goodKey,
			modelsBody:    modelsJSON(goodModel),
			apiKey:        goodKey,
			model:         goodModel,
			role:          RoleChat,
			wantReachable: false,
			wantErr:       true,
			wantErrIs:     ErrUpstream,
		},
		// Non-chat role: step 2 skipped; ToolCalling/Streaming stay false.
		{
			name:            "non_chat_role_step2_skipped",
			serverKey:       goodKey,
			modelsBody:      modelsJSON(goodModel),
			apiKey:          goodKey,
			model:           goodModel,
			role:            RoleEmbeddings,
			wantReachable:   true,
			wantModelServed: true,
			wantToolCalling: false,
			wantStreaming:   false,
		},
		// Empty model list (still 200) — ModelServed=false.
		{
			name:            "empty_model_list",
			serverKey:       goodKey,
			modelsBody:      modelsJSON(),
			apiKey:          goodKey,
			model:           goodModel,
			role:            RoleChat,
			wantReachable:   true,
			wantModelServed: false,
		},

		// (a) Chat endpoint responds with application/json (not SSE): Streaming=false, ToolCalling=false, Err=nil.
		{
			name:   "chat_non_streaming_content_type",
			apiKey: goodKey,
			model:  goodModel,
			role:   RoleChat,
			customHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/models":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, modelsJSON(goodModel))
				case "/v1/chat/completions":
					// Respond with JSON, not SSE — simulates a non-streaming endpoint.
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, `{"choices":[]}`)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}),
			wantReachable:   true,
			wantModelServed: true,
			wantToolCalling: false,
			wantStreaming:   false,
		},

		// (b) Chat endpoint responds with text/event-stream but garbage JSON: Streaming=false, Err wraps
		// ErrUpstreamProtocol.
		{
			name:   "chat_malformed_sse_json",
			apiKey: goodKey,
			model:  goodModel,
			role:   RoleChat,
			customHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/models":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, modelsJSON(goodModel))
				case "/v1/chat/completions":
					// Valid SSE framing, but the data line is not valid JSON.
					writeSSEProbe(w, "data: {not valid json}\n\n")
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}),
			wantReachable:   true,
			wantModelServed: true,
			wantStreaming:   false,
			wantErr:         true,
			wantErrIs:       ErrUpstreamProtocol,
		},

		// (c) Models step succeeds but chat step returns 500: Reachable=true, ModelServed=true, Err wraps ErrUpstream.
		{
			name:   "chat_step_non2xx_500",
			apiKey: goodKey,
			model:  goodModel,
			role:   RoleChat,
			customHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/models":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, modelsJSON(goodModel))
				case "/v1/chat/completions":
					w.WriteHeader(http.StatusInternalServerError)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}),
			wantReachable:   true,
			wantModelServed: true,
			wantToolCalling: false,
			wantStreaming:   false,
			wantErr:         true,
			wantErrIs:       ErrUpstream,
		},

		// (d) Models step returns 500 (non-auth, non-2xx): Reachable=true, Err wraps ErrUpstream, no step 2.
		{
			name:   "models_step_non2xx_500",
			apiKey: goodKey,
			model:  goodModel,
			role:   RoleChat,
			customHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/models":
					w.WriteHeader(http.StatusInternalServerError)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}),
			wantReachable:   true,
			wantModelServed: false,
			wantErr:         true,
			wantErrIs:       ErrUpstream,
		},

		// (e) Models step returns 200 with non-JSON body: Reachable=true, Err non-nil (decode error),
		//     ModelServed=false.
		{
			name:   "models_step_malformed_json",
			apiKey: goodKey,
			model:  goodModel,
			role:   RoleChat,
			customHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/models":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "this is not json")
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}),
			wantReachable:   true,
			wantModelServed: false,
			wantErr:         true,
			wantErrContains: "decode models response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Build the server (or start-then-close for the unreachable case).
			srv := newProbeServer(t, tt.serverKey, tt.modelsBody, tt.chatSSE, tt.customHandler)

			baseURL := srv.URL
			if tt.unreachable {
				// Close the server immediately so the dial fails.
				srv.Close()
			}

			gw := newProbeGateway(t, baseURL, tt.apiKey, tt.model)
			result := gw.Probe(t.Context(), tt.role)

			if result.Reachable != tt.wantReachable {
				t.Errorf("Reachable = %v, want %v", result.Reachable, tt.wantReachable)
			}
			if result.ModelServed != tt.wantModelServed {
				t.Errorf("ModelServed = %v, want %v", result.ModelServed, tt.wantModelServed)
			}
			if result.ToolCalling != tt.wantToolCalling {
				t.Errorf("ToolCalling = %v, want %v", result.ToolCalling, tt.wantToolCalling)
			}
			if result.Streaming != tt.wantStreaming {
				t.Errorf("Streaming = %v, want %v", result.Streaming, tt.wantStreaming)
			}

			if tt.wantErr {
				if result.Err == nil {
					t.Errorf("Err = nil, want non-nil error")
				} else if tt.wantErrIs != nil && !errors.Is(result.Err, tt.wantErrIs) {
					t.Errorf("Err = %v, want errors.Is(err, %v)", result.Err, tt.wantErrIs)
				}
				if tt.wantErrContains != "" && !strings.Contains(result.Err.Error(), tt.wantErrContains) {
					t.Errorf("Err = %q, want substring %q", result.Err.Error(), tt.wantErrContains)
				}
			} else if result.Err != nil {
				t.Errorf("Err = %v, want nil", result.Err)
			}
		})
	}
}

// ── modelsEndpointURL unit tests ──────────────────────────────────────────────

func TestModelsEndpointURL(t *testing.T) {
	t.Parallel()

	const want = "https://api.example.com/v1/models"

	tests := []struct {
		name    string
		rawBase string
		want    string
		wantErr bool
	}{
		{name: "bare host trailing slash", rawBase: "https://api.example.com/", want: want},
		{name: "bare host no trailing slash", rawBase: "https://api.example.com", want: want},
		{name: "with /v1 no trailing slash", rawBase: "https://api.example.com/v1", want: want},
		{name: "with /v1 trailing slash", rawBase: "https://api.example.com/v1/", want: want},
		{
			name:    "subpath with /v1",
			rawBase: "https://proxy.example.com/openai/v1",
			want:    "https://proxy.example.com/openai/v1/models",
		},
		{name: "no scheme", rawBase: "api.example.com/v1", wantErr: true},
		{name: "empty string", rawBase: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := modelsEndpointURL(tt.rawBase)
			if tt.wantErr {
				if err == nil {
					t.Errorf("modelsEndpointURL(%q) = %q, want error", tt.rawBase, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("modelsEndpointURL(%q): unexpected error: %v", tt.rawBase, err)
			}
			if got != tt.want {
				t.Errorf("modelsEndpointURL(%q) = %q, want %q", tt.rawBase, got, tt.want)
			}
		})
	}
}

// TestChatEndpointURL_StillPasses verifies that refactoring chatEndpointURL to call joinEndpointURL did not change its
// observable behaviour.  The canonical test lives in client_test.go; this is a smoke-check that the delegation is
// correct.
func TestChatEndpointURL_AfterRefactor(t *testing.T) {
	t.Parallel()

	got, err := chatEndpointURL("https://api.example.com/v1/")
	if err != nil {
		t.Fatalf("chatEndpointURL: unexpected error: %v", err)
	}
	const want = "https://api.example.com/v1/chat/completions"
	if got != want {
		t.Errorf("chatEndpointURL = %q, want %q", got, want)
	}
}

// ── Concurrent-use safety ─────────────────────────────────────────────────────

// TestGateway_ConcurrentUse locks in the package-doc claim that "each exported type in this package is safe for
// concurrent use". It fires N concurrent [Gateway.Chat] calls and N concurrent [Gateway.Probe] calls against a
// shared Gateway and httptest.Server, fully drains every returned iterator, and relies on -race to surface any
// data races.
//
// The Chat fan-out counts successful drains and asserts all N completed, so a regression that makes every Chat
// setup silently fail cannot produce a false green.
//
// Run with: go test -race ./internal/gateway/...
func TestGateway_ConcurrentUse(t *testing.T) {
	// Do NOT call t.Parallel() — this test's own goroutine fan-out already exercises concurrency and -race catches
	// races within a single test.

	const N = 8 // goroutines per method

	srv := newProbeServer(t, "", modelsJSON("gpt-test"), toolCallSSE, nil)

	gw := newProbeGateway(t, srv.URL, "sk-test", "gpt-test")

	var wg sync.WaitGroup
	var chatDrains atomic.Int32 // counts goroutines that obtained a seq and fully drained it

	// N concurrent Chat calls — each fully drains its iterator.
	for range N {
		wg.Go(func() {
			seq, err := gw.Chat(context.Background(), ChatRequest{
				Messages: []Message{{Role: "user", Content: "hi"}},
			})
			if err != nil {
				return // setup error — counted as not drained
			}
			// Fully drain the iterator (observing any per-yield error) so the gateway goroutine runs to completion.
			for _, streamErr := range seq {
				_ = streamErr
			}
			chatDrains.Add(1)
		})
	}

	// N concurrent Probe calls.
	for range N {
		wg.Go(func() {
			gw.Probe(context.Background(), RoleChat)
		})
	}

	wg.Wait()

	// Every Chat goroutine must have obtained a seq and drained it.
	if got := chatDrains.Load(); got != N {
		t.Errorf("Chat drains = %d, want %d; some goroutines failed to obtain an iterator", got, N)
	}
}
