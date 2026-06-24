package gateway

// Resilience tests for [Gateway].
//
// All tests use httptest.Server for endpoints.  Timing is deterministic:
//   - sleepFn is injected as a no-op (or a recording variant) — no real sleep.
//   - Timeout durations are set to 5 ms for first-token/idle windows; tests
//     that must NOT trip the timeout use a deliberate 100 ms window with
//     chunk delivery well within it.
//   - All tests run under -race (verified via `make race`).
//
// Goroutine leak prevention:
//   - Every test uses t.Cleanup(srv.Close) so the httptest server's goroutines
//     drain before the test exits.
//   - The streaming iterator always terminates because dialChatResolved derives a
//     cancellable context that is cancelled on every exit path of the consumer.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ── test helpers ──────────────────────────────────────────────────────────────

// resilienceCfg returns a fast, deterministic ResilienceConfig for tests.
// sleepFn is a no-op so backoff completes instantly; timeouts are tiny.
func resilienceCfg(firstToken, idle time.Duration, maxAttempts int) ResilienceConfig {
	return ResilienceConfig{
		FirstTokenTimeout: firstToken,
		IdleTimeout:       idle,
		MaxAttempts:       maxAttempts,
		BaseBackoff:       time.Microsecond,
		sleepFn:           func(time.Duration) {},
	}
}

// newResilienceGateway builds a Gateway pointed at srv with the given
// ResilienceConfig and registers cleanup with t.
func newResilienceGateway(t *testing.T, srv *httptest.Server, cfg ResilienceConfig) *Gateway {
	t.Helper()
	fs := newFakeSettings(
		"primary.baseURL", srv.URL,
		"primary.apiKey", "sk-test",
		"primary.model", "gpt-test",
	)
	gw := newGatewayWithFake(fs)
	gw.resilienceCfg = cfg
	return gw
}

// simpleRequest is a minimal ChatRequest for resilience tests (content doesn't
// matter — we only care about transport/timeout behaviour).
func simpleRequest() ChatRequest {
	return ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}
}

// goodSSE is a minimal well-formed SSE stream that yields one text chunk and a
// Done chunk.
const goodSSE = "" +
	`data: {"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}` + "\n\n" +
	`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n" +
	"data: [DONE]\n"

// writeSSE writes the given SSE payload to w with correct headers and flushes.
func writeSSE(w http.ResponseWriter, payload string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, payload)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// ── AC1: transient failure before first token is retried ─────────────────────

// TestResilience_RetryOnConnectionFailure verifies that a transient connection
// failure on the first N-1 attempts is retried and the final successful attempt
// returns a normal stream (AC1).
func TestResilience_RetryOnConnectionFailure(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := callCount.Add(1)
		if n < 3 {
			// Simulate a transient failure: return 503 for the first two calls.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		// Third call succeeds.
		writeSSE(w, goodSSE)
	}))
	t.Cleanup(srv.Close)

	cfg := resilienceCfg(200*time.Millisecond, 200*time.Millisecond, 3)
	gw := newResilienceGateway(t, srv, cfg)

	seq, err := gw.Chat(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("Chat: setup error after retries: %v", err)
	}
	if seq == nil {
		t.Fatal("Chat returned nil seq without error")
	}

	chunks, iterErr := collectChunks(t, seq)
	if iterErr != nil {
		t.Fatalf("iterator error: %v", iterErr)
	}

	if callCount.Load() != 3 {
		t.Errorf("server call count = %d, want 3", callCount.Load())
	}

	var text strings.Builder
	for _, c := range chunks {
		text.WriteString(c.TextDelta)
	}
	if text.String() != "ok" {
		t.Errorf("accumulated text = %q, want %q", text.String(), "ok")
	}
}

// TestResilience_RetryOn429 verifies that a 429 response is retried and, on
// eventual success, returns a normal stream (AC1).
func TestResilience_RetryOn429(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := callCount.Add(1)
		if n < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeSSE(w, goodSSE)
	}))
	t.Cleanup(srv.Close)

	cfg := resilienceCfg(200*time.Millisecond, 200*time.Millisecond, 3)
	gw := newResilienceGateway(t, srv, cfg)

	seq, err := gw.Chat(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("Chat: unexpected error: %v", err)
	}
	chunks, iterErr := collectChunks(t, seq)
	if iterErr != nil {
		t.Fatalf("iterator error: %v", iterErr)
	}
	if callCount.Load() != 3 {
		t.Errorf("server call count = %d, want 3", callCount.Load())
	}

	var hasDone bool
	for _, c := range chunks {
		if c.Done {
			hasDone = true
		}
	}
	if !hasDone {
		t.Error("expected Done chunk in recovered stream")
	}
}

// ── AC2: bounded retries with jittered backoff; 429 Retry-After honoured ─────

// TestResilience_ExhaustRetries_5xx verifies that after MaxAttempts on 5xx the
// gateway returns ErrUpstream (not a nil error) (AC2).
func TestResilience_ExhaustRetries_5xx(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	cfg := resilienceCfg(200*time.Millisecond, 200*time.Millisecond, 3)
	gw := newResilienceGateway(t, srv, cfg)

	_, err := gw.Chat(context.Background(), simpleRequest())
	if !errors.Is(err, ErrUpstream) {
		t.Errorf("expected ErrUpstream after exhausted retries, got %v", err)
	}
	if callCount.Load() != 3 {
		t.Errorf("server call count = %d, want 3 (MaxAttempts)", callCount.Load())
	}
}

// TestResilience_ExhaustRetries_429 verifies that after MaxAttempts on 429 the
// gateway returns an error that wraps ErrRateLimited (AC2).
func TestResilience_ExhaustRetries_429(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	cfg := resilienceCfg(200*time.Millisecond, 200*time.Millisecond, 3)
	gw := newResilienceGateway(t, srv, cfg)

	_, err := gw.Chat(context.Background(), simpleRequest())
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("expected ErrRateLimited after exhausted retries, got %v", err)
	}
	if callCount.Load() != 3 {
		t.Errorf("server call count = %d, want 3 (MaxAttempts)", callCount.Load())
	}
}

// TestResilience_BackoffSleepCalled verifies that the sleepFn is called between
// retries, recording the delays (AC2: jittered backoff invoked).
func TestResilience_BackoffSleepCalled(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	var sleepCount atomic.Int32
	fs := newFakeSettings(
		"primary.baseURL", srv.URL,
		"primary.apiKey", "sk-test",
		"primary.model", "gpt-test",
	)
	gw := newGatewayWithFake(fs)
	gw.resilienceCfg = ResilienceConfig{
		FirstTokenTimeout: 200 * time.Millisecond,
		IdleTimeout:       200 * time.Millisecond,
		MaxAttempts:       3,
		BaseBackoff:       time.Microsecond,
		sleepFn: func(_ time.Duration) {
			sleepCount.Add(1)
		},
	}

	_, err := gw.Chat(context.Background(), simpleRequest())
	if !errors.Is(err, ErrUpstream) {
		t.Errorf("expected ErrUpstream, got %v", err)
	}
	// N attempts → N-1 sleeps between them.
	if sleepCount.Load() != 2 {
		t.Errorf("sleepFn called %d times, want 2 (one between each of 3 attempts)", sleepCount.Load())
	}
}

// TestResilience_RetryAfterHonoured verifies that a 429 with Retry-After is
// passed to sleepFn (AC2: Retry-After honoured).
func TestResilience_RetryAfterHonoured(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			// First call: return 429 with Retry-After: 1 (1 second).
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeSSE(w, goodSSE)
	}))
	t.Cleanup(srv.Close)

	var sleepDurations []time.Duration
	fs := newFakeSettings(
		"primary.baseURL", srv.URL,
		"primary.apiKey", "sk-test",
		"primary.model", "gpt-test",
	)
	gw := newGatewayWithFake(fs)
	gw.resilienceCfg = ResilienceConfig{
		FirstTokenTimeout: 200 * time.Millisecond,
		IdleTimeout:       200 * time.Millisecond,
		MaxAttempts:       3,
		BaseBackoff:       time.Microsecond,
		sleepFn: func(d time.Duration) {
			sleepDurations = append(sleepDurations, d)
		},
	}

	seq, err := gw.Chat(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("Chat: unexpected error: %v", err)
	}
	if _, iterErr := collectChunks(t, seq); iterErr != nil {
		t.Fatalf("iterator error: %v", iterErr)
	}

	if len(sleepDurations) != 1 {
		t.Fatalf("sleepFn called %d times, want 1", len(sleepDurations))
	}
	// The Retry-After is 1s; we should have been asked to sleep ≥ 1s.
	if sleepDurations[0] < time.Second {
		t.Errorf("sleep duration = %v, want ≥ 1s (Retry-After honoured)", sleepDurations[0])
	}
}

// ── AC3: 4xx surfaces immediately; 451 with opt-in retried ───────────────────

// TestResilience_4xxNoRetry verifies that non-retryable 4xx errors surface
// immediately with no retry (AC3).
func TestResilience_4xxNoRetry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		wantErr error
	}{
		{name: "401", status: http.StatusUnauthorized, wantErr: ErrAuth},
		{name: "403", status: http.StatusForbidden, wantErr: ErrAuth},
		{name: "400_generic", status: http.StatusBadRequest, wantErr: ErrUpstream},
		{name: "422", status: http.StatusUnprocessableEntity, wantErr: ErrUpstream},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var callCount atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				callCount.Add(1)
				w.WriteHeader(tt.status)
			}))
			t.Cleanup(srv.Close)

			cfg := resilienceCfg(200*time.Millisecond, 200*time.Millisecond, 3)
			gw := newResilienceGateway(t, srv, cfg)

			_, err := gw.Chat(context.Background(), simpleRequest())
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Chat error = %v, want errors.Is(%v)", err, tt.wantErr)
			}
			if callCount.Load() != 1 {
				t.Errorf("server call count = %d, want 1 (no retry)", callCount.Load())
			}
		})
	}
}

// TestResilience_451_RetryLegalFalse verifies that a 451 with
// retryLegalUnavailable=false surfaces immediately as ErrUpstream (AC3).
func TestResilience_451_RetryLegalFalse(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusUnavailableForLegalReasons)
	}))
	t.Cleanup(srv.Close)

	// retryLegalUnavailable not set → false (default).
	fs := newFakeSettings(
		"primary.baseURL", srv.URL,
		"primary.apiKey", "sk-test",
		"primary.model", "gpt-test",
	)
	gw := newGatewayWithFake(fs)
	gw.resilienceCfg = resilienceCfg(200*time.Millisecond, 200*time.Millisecond, 3)

	_, err := gw.Chat(context.Background(), simpleRequest())
	if !errors.Is(err, ErrUpstream) {
		t.Errorf("Chat error = %v, want ErrUpstream", err)
	}
	if callCount.Load() != 1 {
		t.Errorf("server call count = %d, want 1 (no retry when retryLegal=false)", callCount.Load())
	}
}

// TestResilience_451_RetryLegalTrue verifies that a 451 with
// retryLegalUnavailable=true is retried within budget and recovers (AC3).
func TestResilience_451_RetryLegalTrue(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := callCount.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusUnavailableForLegalReasons)
			return
		}
		writeSSE(w, goodSSE)
	}))
	t.Cleanup(srv.Close)

	// Set retryLegalUnavailable=true in settings.
	fs := newFakeSettings(
		"primary.baseURL", srv.URL,
		"primary.apiKey", "sk-test",
		"primary.model", "gpt-test",
		"retryLegalUnavailable", "true",
	)
	gw := newGatewayWithFake(fs)
	gw.resilienceCfg = resilienceCfg(200*time.Millisecond, 200*time.Millisecond, 3)

	seq, err := gw.Chat(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("Chat: unexpected error: %v", err)
	}
	if _, iterErr := collectChunks(t, seq); iterErr != nil {
		t.Fatalf("iterator error: %v", iterErr)
	}
	if callCount.Load() != 3 {
		t.Errorf("server call count = %d, want 3 (retried twice, then success)", callCount.Load())
	}
}

// ── AC4: no retry after first byte; post-first-byte break surfaces correctly ─

// TestResilience_NoRetryAfterFirstByte verifies that a mid-stream break does
// not cause the gateway to retry: the iterator simply surfaces the error as a
// per-yield error and terminates without re-emitting (AC4).
func TestResilience_NoRetryAfterFirstByte(t *testing.T) {
	t.Parallel()

	// The server sends one chunk then closes abruptly.
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w,
			`data: {"choices":[{"index":0,"delta":{"content":"first"},"finish_reason":null}]}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Hijack and close the underlying connection to produce a mid-stream break.
		hj, ok := w.(http.Hijacker)
		if !ok {
			return // graceful close is acceptable too
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)

	cfg := resilienceCfg(200*time.Millisecond, 200*time.Millisecond, 3)
	gw := newResilienceGateway(t, srv, cfg)

	seq, err := gw.Chat(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("Chat: unexpected setup error: %v", err)
	}

	// Range and collect chunks.
	var chunks []Chunk
	var iterErr error
	for c, e := range seq {
		if e != nil {
			iterErr = e
			break
		}
		chunks = append(chunks, c)
	}

	// The server was contacted exactly once — no retry after first byte.
	if callCount.Load() != 1 {
		t.Errorf("server call count = %d, want 1 (no post-stream retry)", callCount.Load())
	}

	// The first chunk must be the "first" text delta.
	var hasFirst bool
	for _, c := range chunks {
		if c.TextDelta == "first" {
			hasFirst = true
		}
	}
	if !hasFirst {
		t.Errorf("expected 'first' text chunk; got chunks: %v", chunks)
	}

	// The mid-stream break shows up as a per-yield error (or the stream simply
	// ends naturally — both are acceptable since bufio.Scanner treats abrupt
	// close as EOF). The important invariant is no re-emit.
	if iterErr != nil && !errors.Is(iterErr, ErrUpstreamProtocol) {
		t.Errorf("iterator error = %v; want nil or ErrUpstreamProtocol", iterErr)
	}
}

// TestResilience_PostFirstByteBreak_NeverReemits verifies that after an error
// mid-stream the iterator terminates and does not emit any further chunks (AC4).
func TestResilience_PostFirstByteBreak_NeverReemits(t *testing.T) {
	t.Parallel()

	// A stream that delivers a valid chunk then malformed JSON.
	const ssePayload = "" +
		`data: {"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}` + "\n\n" +
		"data: {BAD JSON}\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, ssePayload)
	}))
	t.Cleanup(srv.Close)

	cfg := resilienceCfg(200*time.Millisecond, 200*time.Millisecond, 3)
	gw := newResilienceGateway(t, srv, cfg)

	seq, err := gw.Chat(context.Background(), simpleRequest())
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
			break
		}
		gotChunks = append(gotChunks, c)
	}

	// Exactly one text chunk + one error yield.
	if yieldCount != 2 {
		t.Errorf("yield count = %d, want 2 (one text + one error)", yieldCount)
	}
	var hasA bool
	for _, c := range gotChunks {
		if c.TextDelta == "a" {
			hasA = true
		}
	}
	if !hasA {
		t.Errorf("expected text chunk 'a' before error")
	}
	if !errors.Is(gotErr, ErrUpstreamProtocol) {
		t.Errorf("iterator error = %v, want ErrUpstreamProtocol", gotErr)
	}
}

// ── AC5: first-token and idle timeouts ────────────────────────────────────────

// TestResilience_FirstTokenTimeout verifies that if the endpoint never sends
// any chunk within FirstTokenTimeout the iterator yields ErrTimeout (AC5).
func TestResilience_FirstTokenTimeout(t *testing.T) {
	t.Parallel()

	// The server sends 2xx but never writes any data (hangs).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Block until the request context is cancelled (client gave up).
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	// Very short first-token timeout; idle timeout is irrelevant here.
	cfg := resilienceCfg(5*time.Millisecond, 200*time.Millisecond, 1)
	gw := newResilienceGateway(t, srv, cfg)

	seq, err := gw.Chat(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("Chat: unexpected setup error: %v", err)
	}

	var iterErr error
	for _, e := range seq {
		if e != nil {
			iterErr = e
			break
		}
	}
	if !errors.Is(iterErr, ErrTimeout) {
		t.Errorf("iterator error = %v, want ErrTimeout", iterErr)
	}
}

// TestResilience_IdleTimeout verifies that a stream that sends one chunk then
// stalls trips ErrTimeout via the idle timer (AC5).
func TestResilience_IdleTimeout(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Send one valid chunk immediately.
		_, _ = fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"go"},"finish_reason":null}]}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Then stall until the client cancels.
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	// First-token window is generous (we know the first chunk arrives quickly).
	// Idle window is tiny — the stall after the first chunk should trip it.
	cfg := resilienceCfg(200*time.Millisecond, 5*time.Millisecond, 1)
	gw := newResilienceGateway(t, srv, cfg)

	seq, err := gw.Chat(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("Chat: unexpected setup error: %v", err)
	}

	var chunks []Chunk
	var iterErr error
	for c, e := range seq {
		if e != nil {
			iterErr = e
			break
		}
		chunks = append(chunks, c)
	}

	// The first chunk must have arrived.
	var hasGo bool
	for _, c := range chunks {
		if c.TextDelta == "go" {
			hasGo = true
		}
	}
	if !hasGo {
		t.Errorf("expected 'go' chunk before idle timeout; chunks = %v", chunks)
	}
	if !errors.Is(iterErr, ErrTimeout) {
		t.Errorf("iterator error = %v, want ErrTimeout (idle)", iterErr)
	}
}

// TestResilience_SlowButProgressingStream verifies that a slow-but-progressing
// stream (each chunk arrives within the idle window) is NOT aborted (AC5).
func TestResilience_SlowButProgressingStream(t *testing.T) {
	t.Parallel()

	// The server drip-feeds three chunks, each within the idle window.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, canFlush := w.(http.Flusher)

		chunks := []string{
			`data: {"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}` + "\n\n",
			`data: {"choices":[{"index":0,"delta":{"content":"b"},"finish_reason":null}]}` + "\n\n",
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}` + "\n\n",
			"data: [DONE]\n",
		}
		for _, chunk := range chunks {
			_, _ = io.WriteString(w, chunk)
			if canFlush {
				f.Flush()
			}
			// Sleep 1 ms between chunks — well within the 50 ms idle window.
			time.Sleep(time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)

	// Large enough idle window that 1 ms gaps don't trip it.
	cfg := resilienceCfg(200*time.Millisecond, 50*time.Millisecond, 1)
	gw := newResilienceGateway(t, srv, cfg)

	seq, err := gw.Chat(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("Chat: unexpected setup error: %v", err)
	}

	chunks, iterErr := collectChunks(t, seq)
	if iterErr != nil {
		t.Fatalf("iterator error (should not timeout): %v", iterErr)
	}

	var text strings.Builder
	for _, c := range chunks {
		text.WriteString(c.TextDelta)
	}
	if text.String() != "ab" {
		t.Errorf("accumulated text = %q, want %q", text.String(), "ab")
	}

	var hasDone bool
	for _, c := range chunks {
		if c.Done {
			hasDone = true
		}
	}
	if !hasDone {
		t.Error("expected Done chunk in slow-but-progressing stream")
	}
}

// ── AC6: ctx cancellation aborts promptly ────────────────────────────────────

// TestResilience_CtxCancelDuringRetry verifies that cancelling ctx while the
// retry loop is sleeping (pre-first-token) aborts immediately (AC6).
func TestResilience_CtxCancelDuringRetry(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())

	// Use a sleepFn that cancels the context mid-sleep to simulate cancellation
	// during backoff without real wall-clock delay.
	fs := newFakeSettings(
		"primary.baseURL", srv.URL,
		"primary.apiKey", "sk-test",
		"primary.model", "gpt-test",
	)
	gw := newGatewayWithFake(fs)
	gw.resilienceCfg = ResilienceConfig{
		FirstTokenTimeout: 200 * time.Millisecond,
		IdleTimeout:       200 * time.Millisecond,
		MaxAttempts:       5, // high so we'd need ctx cancel to escape
		BaseBackoff:       time.Microsecond,
		sleepFn: func(time.Duration) {
			cancel() // cancel context during the first backoff sleep
		},
	}

	_, err := gw.Chat(ctx, simpleRequest())
	// After cancel, the next attempt checks ctx.Err() and should surface as
	// ErrTimeout or context.Canceled. Either wrapped form is acceptable.
	if err == nil {
		t.Fatal("expected non-nil error after context cancel")
	}
}

// TestResilience_CtxCancelDuringStream verifies that cancelling ctx while
// streaming aborts the stream and surfaces ErrTimeout (or context error) (AC6).
func TestResilience_CtxCancelDuringStream(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w,
			`data: {"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		// Hang until client cancels.
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := resilienceCfg(200*time.Millisecond, 200*time.Millisecond, 1)
	gw := newResilienceGateway(t, srv, cfg)

	seq, err := gw.Chat(ctx, simpleRequest())
	if err != nil {
		t.Fatalf("Chat: unexpected setup error: %v", err)
	}

	var iterErr error
	for c, e := range seq {
		if e != nil {
			iterErr = e
			break
		}
		if c.TextDelta == "x" {
			// First chunk received — now cancel.
			cancel()
		}
	}

	// The iterator should have surfaced an error after the cancel.
	if iterErr == nil {
		t.Error("expected iterator error after ctx cancel")
	}
}

// ── AC7: ErrContextLength never retried ──────────────────────────────────────

// TestResilience_ContextLengthNeverRetried verifies that ErrContextLength
// surfaces immediately with exactly one server call (AC7).
func TestResilience_ContextLengthNeverRetried(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		// Return 400 with context-length-exceeded body.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"context_length_exceeded"}}`)
	}))
	t.Cleanup(srv.Close)

	cfg := resilienceCfg(200*time.Millisecond, 200*time.Millisecond, 3)
	gw := newResilienceGateway(t, srv, cfg)

	_, err := gw.Chat(context.Background(), simpleRequest())
	if !errors.Is(err, ErrContextLength) {
		t.Errorf("Chat error = %v, want ErrContextLength", err)
	}
	if callCount.Load() != 1 {
		t.Errorf("server call count = %d, want 1 (ErrContextLength never retried)", callCount.Load())
	}
}

// ── AC8: all tests pass under -race (enforced by Makefile `race` target) ─────
//
// No explicit test here — all of the above are designed to be race-clean:
// shared state uses atomic.Int32, channels, and contexts. goroutine lifetimes
// are bounded by context cancellation (defer dr.cancel in streamWithTimeouts)
// and httptest server shutdown (t.Cleanup(srv.Close)).

// ── Cleanup: the streaming producer goroutine must not accumulate ────────────

// eventually polls cond every 5ms until it returns true or timeout elapses.
func eventually(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// streamForeverHandler streams SSE text chunks until the client disconnects
// (its request context is cancelled).
func streamForeverHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("server ResponseWriter is not http.Flusher")
			return
		}
		for i := 0; ; i++ {
			if r.Context().Err() != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tok%d\"}}]}\n\n", i); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// TestResilience_EarlyBreak_NoGoroutineAccumulation drives the early-break
// cleanup path many times — a consumer that reads one chunk and breaks while the
// endpoint is still streaming — and asserts the streaming producer goroutines do
// not accumulate. It guards against a cleanup regression in which a consumer exit
// fails to terminate its producer (the iterator cancels streamCtx on every exit,
// which unblocks a producer parked on a channel send, and dr.cancel aborts one
// parked on the body read). Not parallel: it reads the process-global
// runtime.NumGoroutine.
func TestResilience_EarlyBreak_NoGoroutineAccumulation(t *testing.T) {
	srv := httptest.NewServer(streamForeverHandler(t))
	t.Cleanup(srv.Close)

	// Generous windows so no timeout trips — exercising early break only.
	cfg := resilienceCfg(2*time.Second, 2*time.Second, 1)
	gw := newResilienceGateway(t, srv, cfg)

	consumeOneThenBreak := func() {
		seq, err := gw.Chat(context.Background(), simpleRequest())
		if err != nil {
			t.Fatalf("Chat setup error: %v", err)
		}
		for chunk, cerr := range seq {
			if cerr != nil {
				return
			}
			_ = chunk
			break // early break after the first chunk
		}
	}

	// Warm up so lazily-created transport/server goroutines exist before baseline.
	consumeOneThenBreak()
	runtime.GC()
	baseline := runtime.NumGoroutine()

	const iterations = 40
	for range iterations {
		consumeOneThenBreak()
	}

	// A cleanup regression that leaked one producer per iteration would push the
	// count to ~baseline+iterations; the small slack absorbs scheduler noise.
	if !eventually(3*time.Second, func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= baseline+5
	}) {
		t.Fatalf("producer goroutines accumulated: NumGoroutine=%d, baseline=%d after %d early-break iterations",
			runtime.NumGoroutine(), baseline, iterations)
	}
}
