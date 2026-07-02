package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qovira/qovira/internal/httpx"
)

// TestSecurityHeadersMiddleware_NosniffOnNormalResponse verifies that a request served through
// NewSecurityHeadersMiddleware produces a response with X-Content-Type-Options: nosniff, regardless
// of the inner handler's status code.
func TestSecurityHeadersMiddleware_NosniffOnNormalResponse(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := httpx.NewSecurityHeadersMiddleware(inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	got := rr.Header().Get("X-Content-Type-Options")
	if got != "nosniff" {
		t.Errorf("X-Content-Type-Options: want %q, got %q", "nosniff", got)
	}
}

// TestSecurityHeadersMiddleware_NosniffWhenInnerWritesStatus verifies that X-Content-Type-Options: nosniff
// is present even when the inner handler writes its own non-200 status code (e.g. a 404 or 204). The header
// must be set before ServeHTTP is called so it appears on every response shape.
func TestSecurityHeadersMiddleware_NosniffWhenInnerWritesStatus(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusNoContent, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
			h := httpx.NewSecurityHeadersMiddleware(inner)

			req := httptest.NewRequest(http.MethodGet, "/anything", nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != status {
				t.Errorf("status: want %d, got %d", status, rr.Code)
			}

			got := rr.Header().Get("X-Content-Type-Options")
			if got != "nosniff" {
				t.Errorf("X-Content-Type-Options: want %q, got %q", "nosniff", got)
			}
		})
	}
}
