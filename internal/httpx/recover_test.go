package httpx_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qovira/qovira/internal/httpx"
)

// discardLogger returns a slog.Logger that discards output — recovery's logging is a side effect we don't
// assert on in these tests, so we just keep it off the test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRecoveryMiddleware_PanicYields500 verifies that a panicking handler results in a generic 500 response
// in the house problem+json shape, with no stack trace or panic value in the body and no connection drop.
func TestRecoveryMiddleware_PanicYields500(t *testing.T) {
	t.Parallel()

	panicker := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("something went wrong")
	})

	// Compose: request-ID first so recovery can read the ID from context for logging.
	h := httpx.NewRequestIDMiddleware(httpx.NewRecoveryMiddleware(discardLogger(), panicker))

	req := httptest.NewRequest(http.MethodGet, "/crash", nil)
	rr := httptest.NewRecorder()

	// The test itself would panic without recovery — the middleware must catch it.
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("panic handler: want 500, got %d", rr.Code)
	}

	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type: want %q, got %q", "application/problem+json", ct)
	}

	body, _ := io.ReadAll(rr.Body)
	bodyStr := string(body)

	// Ensure none of the panic value or a stack trace leaks into the response.
	if contains(bodyStr, "something went wrong") {
		t.Errorf("500 body leaks panic value: %q", bodyStr)
	}

	if contains(bodyStr, "goroutine") {
		t.Errorf("500 body contains stack trace: %q", bodyStr)
	}

	// The body must be the house RFC 9457 problem with the internal_error code and a 500 status.
	var problem struct {
		Status int    `json:"status"`
		Code   string `json:"code"`
	}

	if err := json.Unmarshal(body, &problem); err != nil {
		t.Fatalf("500 body is not valid problem+json: %v (body: %q)", err, bodyStr)
	}

	if problem.Status != http.StatusInternalServerError {
		t.Errorf("problem status field: want 500, got %d", problem.Status)
	}

	if problem.Code != "internal_error" {
		t.Errorf("problem code: want %q, got %q", "internal_error", problem.Code)
	}
}

// TestRecoveryMiddleware_NoPanicPassthrough verifies that a non-panicking handler's status and body pass
// through the recovery middleware unmodified.
func TestRecoveryMiddleware_NoPanicPassthrough(t *testing.T) {
	t.Parallel()

	normal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("I'm a teapot"))
	})

	h := httpx.NewRecoveryMiddleware(discardLogger(), normal)

	req := httptest.NewRequest(http.MethodGet, "/teapot", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusTeapot {
		t.Errorf("non-panic handler: want 418, got %d", rr.Code)
	}

	body, _ := io.ReadAll(rr.Body)
	if string(body) != "I'm a teapot" {
		t.Errorf("body: want \"I'm a teapot\", got %q", string(body))
	}
}

// TestRecoveryMiddleware_PanicSetsRequestID verifies that the response from a recovered panic carries the
// Request-Id header (set by the outer request-ID middleware) and that the same ID appears in the body.
func TestRecoveryMiddleware_PanicSetsRequestID(t *testing.T) {
	t.Parallel()

	panicker := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom")
	})

	// Outer: request-ID → inner: recovery → panicker.
	h := httpx.NewRequestIDMiddleware(httpx.NewRecoveryMiddleware(discardLogger(), panicker))

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rr.Code)
	}

	headerID := rr.Header().Get("Request-Id")
	if headerID == "" {
		t.Fatal("Request-Id header missing from panic-recovered 500 response")
	}

	var problem struct {
		RequestID string `json:"requestId"`
	}

	if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
		t.Fatalf("500 body is not valid problem+json: %v", err)
	}

	if problem.RequestID != headerID {
		t.Errorf("requestId mismatch: header %q, body %q", headerID, problem.RequestID)
	}
}

// TestRecoveryMiddleware_RePanicsErrAbortHandler verifies that http.ErrAbortHandler — the stdlib sentinel a
// handler panics with to abort the response silently — is re-panicked (so net/http handles the abort as
// designed) rather than swallowed into a logged 500.
func TestRecoveryMiddleware_RePanicsErrAbortHandler(t *testing.T) {
	t.Parallel()

	aborter := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic(http.ErrAbortHandler)
	})

	h := httpx.NewRecoveryMiddleware(discardLogger(), aborter)

	req := httptest.NewRequest(http.MethodGet, "/abort", nil)
	rr := httptest.NewRecorder()

	defer func() {
		switch v := recover(); v {
		case http.ErrAbortHandler:
			// Expected: the abort sentinel propagated out for net/http to handle.
		case nil:
			t.Error("ErrAbortHandler was swallowed; want it re-panicked")
		default:
			t.Errorf("re-panicked with %v; want http.ErrAbortHandler", v)
		}
	}()

	h.ServeHTTP(rr, req)
	t.Fatal("ServeHTTP returned; expected ErrAbortHandler to propagate")
}

// TestRecoveryMiddleware_NoDoubleWriteAfterCommit verifies that when a handler has already committed a
// response (wrote a status + partial body) before panicking, recovery does NOT write a second status/body
// over it — the committed bytes are left intact. The full access-log→recovery chain is used so recovery's
// writer is the commit-tracking statusRecorder.
func TestRecoveryMiddleware_NoDoubleWriteAfterCommit(t *testing.T) {
	t.Parallel()

	streamer := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("mid-stream boom")
	})

	h := httpx.NewAccessLogMiddleware(discardLogger(), httpx.NewRecoveryMiddleware(discardLogger(), streamer))

	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("committed status: want 200 (recovery must not overwrite), got %d", rr.Code)
	}

	// The body must be exactly the partial write — recovery must not have appended a 500 problem body.
	if got := rr.Body.String(); got != "partial" {
		t.Errorf("body: want %q (no recovery write over committed response), got %q", "partial", got)
	}
}

// contains is a helper for substring checks (also used by cors_test.go via containsCaseInsensitive).
func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}

	if len(s) < len(sub) {
		return false
	}

	for i := range len(s) - len(sub) + 1 {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}

	return false
}
