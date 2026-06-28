package httpx_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qovira/qovira/internal/httpx"
)

// TestRecoveryMiddleware_PanicYields500 verifies that a panicking handler results in a generic 500 response
// with no stack trace in the body and no connection drop.
func TestRecoveryMiddleware_PanicYields500(t *testing.T) {
	t.Parallel()

	panicker := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("something went wrong")
	})

	// Compose: request-ID first so recovery can read the ID from context for logging.
	h := httpx.NewRequestIDMiddleware(httpx.NewRecoveryMiddleware(panicker))

	req := httptest.NewRequest(http.MethodGet, "/crash", nil)
	rr := httptest.NewRecorder()

	// The test itself would panic without recovery — the middleware must catch it.
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("panic handler: want 500, got %d", rr.Code)
	}

	body, _ := io.ReadAll(rr.Body)
	bodyStr := string(body)

	// The body must not contain a stack trace — we just need a generic message.
	if bodyStr == "" {
		t.Error("500 body is empty; expect a brief generic message")
	}

	// Ensure none of the panic value leaks into the response.
	if contains(bodyStr, "something went wrong") {
		t.Errorf("500 body leaks panic value: %q", bodyStr)
	}

	// goroutine stacks must not appear.
	if contains(bodyStr, "goroutine") {
		t.Errorf("500 body contains stack trace: %q", bodyStr)
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

	h := httpx.NewRecoveryMiddleware(normal)

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

// TestRecoveryMiddleware_PanicSetsRequestID verifies that the response from a recovered panic still carries
// the Request-Id header (set by the outer request-ID middleware before the panic fires).
func TestRecoveryMiddleware_PanicSetsRequestID(t *testing.T) {
	t.Parallel()

	panicker := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom")
	})

	// Outer: request-ID → inner: recovery → panicker.
	// The access-log wrapper is not in this chain; we just verify the header survives.
	h := httpx.NewRequestIDMiddleware(httpx.NewRecoveryMiddleware(panicker))

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rr.Code)
	}

	if id := rr.Header().Get("Request-Id"); id == "" {
		t.Error("Request-Id header missing from panic-recovered 500 response")
	}
}

// contains is a helper for substring checks.
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
