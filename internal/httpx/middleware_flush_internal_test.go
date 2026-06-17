package httpx

// White-box regression tests for the ResponseWriter wrappers' streaming contract.
// They live in package httpx (not httpx_test) so they can reference the unexported
// responseGuard / statusRecorder wrappers directly.
//
// The SSE handler (eventsHandler) drives its connection through
// http.ResponseController.Flush. That controller reaches the underlying flusher
// only by walking an Unwrap() http.ResponseWriter method on each wrapper in the
// chain. Both responseGuard (RecoverMiddleware) and statusRecorder
// (RequestLogMiddleware) embed the http.ResponseWriter *interface*, which promotes
// only Header/Write/WriteHeader — never Flush. So without an explicit Unwrap the
// controller dead-ends at the wrapper, Flush returns ErrNotSupported, and the
// events handler bails before streaming a single frame (the stream looks "open"
// to the client, then closes immediately). These tests pin the Unwrap contract so
// the live SSE path cannot silently regress.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResponseGuard_UnwrapReachesFlusher(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder() // concrete type implements http.Flusher
	g := &responseGuard{ResponseWriter: rec}

	if err := http.NewResponseController(g).Flush(); err != nil {
		t.Fatalf("Flush through responseGuard: %v — the wrapper must expose Unwrap() so SSE streaming reaches the flusher", err)
	}
}

func TestStatusRecorder_UnwrapReachesFlusher(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	s := &statusRecorder{ResponseWriter: rec}

	if err := http.NewResponseController(s).Flush(); err != nil {
		t.Fatalf("Flush through statusRecorder: %v — the wrapper must expose Unwrap() so SSE streaming reaches the flusher", err)
	}
}

// TestNestedWrappers_UnwrapReachesFlusher mirrors the production composition
// (RecoverMiddleware outside RequestLogMiddleware): the controller must unwrap
// transitively through both wrappers to reach the real flusher.
func TestNestedWrappers_UnwrapReachesFlusher(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	g := &responseGuard{ResponseWriter: rec}
	s := &statusRecorder{ResponseWriter: g}

	if err := http.NewResponseController(s).Flush(); err != nil {
		t.Fatalf("Flush through statusRecorder→responseGuard: %v — both wrappers must expose Unwrap()", err)
	}
}
