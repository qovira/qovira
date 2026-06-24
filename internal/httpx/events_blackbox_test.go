package httpx_test

// Black-box tests for the /events endpoint that need helpers from the httpx_test package (newServerHandler,
// problemBody). The white-box tests that call unexported symbols (eventsHandler, writeSSEEvent, writeSSEPing) stay in
// events_test.go (package httpx).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestEventsHandler_401SlugIsUnauthenticated verifies that the 401 returned by the /events handler uses the code slug
// "unauthenticated", matching the slug used everywhere else in the API for missing/invalid auth. The old code used
// "unauthorized" which was inconsistent with AuthMiddleware and the HTTP guide.
func TestEventsHandler_401SlugIsUnauthenticated(t *testing.T) {
	t.Parallel()

	h := newServerHandler(t, "dev")

	// GET /events with no principal in context → 401.
	r := httptest.NewRequest(http.MethodGet, "/events", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}

	var p problemBody
	if err := json.NewDecoder(rr.Body).Decode(&p); err != nil {
		t.Fatalf("decode problem body: %v", err)
	}
	if p.Code != "unauthenticated" {
		t.Errorf("problem.code = %q, want %q", p.Code, "unauthenticated")
	}
}
