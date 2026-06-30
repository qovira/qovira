package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/qovira/qovira/internal/httpx"
)

// reqIDPattern matches the req_<Crockford-base32> shape: "req_" followed by 1 or more characters from
// the Crockford alphabet (0-9 A-Z excluding I L O U, case-insensitive). Token length is not fixed so the
// test is resilient to implementation changes in the random byte count.
var reqIDPattern = regexp.MustCompile(`^req_[0-9A-HJ-NP-TV-Z]+$`)

// TestRequestIDMiddleware_GeneratesTokenWhenAbsent verifies that the middleware generates a req_ token
// when no Request-Id is present in the incoming request.
func TestRequestIDMiddleware_GeneratesTokenWhenAbsent(t *testing.T) {
	t.Parallel()

	var captured string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = httpx.RequestID(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	h := httpx.NewRequestIDMiddleware(inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	got := rr.Header().Get("Request-Id")
	if got == "" {
		t.Fatal("Request-Id header missing from response")
	}

	if !reqIDPattern.MatchString(got) {
		t.Errorf("Request-Id %q does not match req_<Crockford-base32> pattern", got)
	}

	if captured != got {
		t.Errorf("context request ID %q differs from response header %q", captured, got)
	}
}

// TestRequestIDMiddleware_EchoesWellFormedInbound verifies that a well-formed inbound Request-Id is echoed
// back on the response and placed in context unchanged.
func TestRequestIDMiddleware_EchoesWellFormedInbound(t *testing.T) {
	t.Parallel()

	const inbound = "req_8D2F1C"

	var captured string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = httpx.RequestID(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	h := httpx.NewRequestIDMiddleware(inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Request-Id", inbound)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	got := rr.Header().Get("Request-Id")
	if got != inbound {
		t.Errorf("echoed Request-Id: want %q, got %q", inbound, got)
	}

	if captured != inbound {
		t.Errorf("context request ID: want %q, got %q", inbound, captured)
	}
}

// TestRequestIDMiddleware_ReplacesOversizedInbound verifies that an inbound Request-Id that exceeds the
// length bound is rejected and replaced with a generated req_ token.
func TestRequestIDMiddleware_ReplacesOversizedInbound(t *testing.T) {
	t.Parallel()

	// Build a value longer than the allowed maximum (200 chars is well over any reasonable limit).
	oversized := "req_" + string(make([]byte, 200))

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := httpx.NewRequestIDMiddleware(inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Request-Id", oversized)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	got := rr.Header().Get("Request-Id")
	if got == oversized {
		t.Error("oversized inbound Request-Id was echoed; expected replacement with generated token")
	}

	if !reqIDPattern.MatchString(got) {
		t.Errorf("replacement Request-Id %q does not match req_<Crockford-base32> pattern", got)
	}
}

// TestRequestIDMiddleware_ReplacesUnsafeCharset verifies that an inbound Request-Id with characters
// outside the safe charset is rejected and replaced with a generated req_ token.
func TestRequestIDMiddleware_ReplacesUnsafeCharset(t *testing.T) {
	t.Parallel()

	// Contains a newline — the classic log-injection payload.
	malformed := "req_abc\r\nX-Injected: evil"

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := httpx.NewRequestIDMiddleware(inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Request-Id", malformed)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	got := rr.Header().Get("Request-Id")
	if got == malformed {
		t.Error("malformed inbound Request-Id was echoed; expected replacement with generated token")
	}

	if !reqIDPattern.MatchString(got) {
		t.Errorf("replacement Request-Id %q does not match req_<Crockford-base32> pattern", got)
	}
}

// TestRequestIDMiddleware_TokenIsUnique verifies that two consecutive requests get different generated tokens.
func TestRequestIDMiddleware_TokenIsUnique(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := httpx.NewRequestIDMiddleware(inner)

	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	rr1 := httptest.NewRecorder()
	h.ServeHTTP(rr1, req1)

	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)

	id1 := rr1.Header().Get("Request-Id")
	id2 := rr2.Header().Get("Request-Id")

	if id1 == id2 {
		t.Errorf("two generated tokens are identical: %q — crypto/rand must be used", id1)
	}
}

// TestRequestID_EmptyContextReturnsEmpty verifies that httpx.RequestID returns "" when the context carries
// no request ID (i.e. the middleware was not in the chain).
func TestRequestID_EmptyContextReturnsEmpty(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	got := httpx.RequestID(req.Context())

	if got != "" {
		t.Errorf("expected empty string from bare context, got %q", got)
	}
}
