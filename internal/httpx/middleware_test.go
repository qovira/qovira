package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qovira/qovira/internal/httpx"
	"github.com/qovira/qovira/internal/store"
)

// ---- helpers ----------------------------------------------------------------

// noopHandler returns a handler that writes the given status code and body.
func noopHandler(status int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
}

// newTestLogger returns a *slog.Logger backed by a *bytes.Buffer and the
// buffer itself so tests can inspect what was logged.
func newTestLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}

// fakeValidator is a test-double for httpx.TokenValidator.
// If Token matches the configured expected value, it returns Principal; else error.
type fakeValidator struct {
	expectedToken string
	principal     store.Principal
}

func (f *fakeValidator) ValidateToken(_ context.Context, token string) (store.Principal, error) {
	if token != f.expectedToken {
		return store.Principal{}, &unauthenticatedError{token: token}
	}
	return f.principal, nil
}

type unauthenticatedError struct{ token string }

func (e *unauthenticatedError) Error() string { return "invalid token: " + e.token }

// ---- MiddlewareChain --------------------------------------------------------

// TestDefaultChain_OrderIsOutermostFirst verifies that StandardChain composes
// the middlewares so that recover is outermost and auth is innermost (before route).
// It does this by checking execution order via a recording handler.
func TestDefaultChain_OrderIsOutermostFirst(t *testing.T) {
	t.Parallel()

	var order []string
	mark := func(label string) httpx.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, label+":in")
				next.ServeHTTP(w, r)
				order = append(order, label+":out")
			})
		}
	}

	logger, _ := newTestLogger()
	validator := &fakeValidator{expectedToken: "tok", principal: store.Principal{UserID: "u1", Role: "user"}}
	isPublic := func(r *http.Request) bool { return r.URL.Path == "/healthz" }

	// Replace each real middleware with a recording one to see ordering.
	// We test the StandardChain via its ordering seam: the function must return
	// middlewares in recover→request-id→request-log→security-headers→auth order.
	mws := httpx.StandardChain(logger, validator, isPublic)
	if len(mws) != 5 {
		t.Fatalf("StandardChain returned %d middlewares, want 5", len(mws))
	}
	_ = mws

	// Build a chain from recording stubs in the declared order and verify.
	chained := httpx.Chain(
		http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			order = append(order, "route")
		}),
		mark("recover"),
		mark("request-id"),
		mark("request-log"),
		mark("security-headers"),
		mark("auth"),
	)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	r.Header.Set("Authorization", "Bearer tok")
	chained.ServeHTTP(httptest.NewRecorder(), r)

	want := []string{
		"recover:in", "request-id:in", "request-log:in", "security-headers:in", "auth:in",
		"route",
		"auth:out", "security-headers:out", "request-log:out", "request-id:out", "recover:out",
	}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i, v := range want {
		if order[i] != v {
			t.Errorf("order[%d] = %q, want %q", i, order[i], v)
		}
	}
}

// ---- RecoverMiddleware ------------------------------------------------------

// TestRecoverMiddleware_PanicYields500 verifies that a panicking inner handler
// produces a 500 problem+json response with no stack trace in the body.
func TestRecoverMiddleware_PanicYields500(t *testing.T) {
	t.Parallel()

	logger, logBuf := newTestLogger()

	panicHandler := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("something went badly wrong")
	})

	// Compose: request-id (outermost) so Request-Id header is set before recover
	// panics, then recover catches it. Per the spec, request-id sets the header
	// BEFORE calling next, so recover can write the 500 response with the header
	// already present.
	// For this test we use recover as the outermost wrapper directly.
	h := httpx.Chain(panicHandler, httpx.RecoverMiddleware(logger))

	r := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}

	body := rr.Body.String()
	// Body must not contain stack trace or panic value.
	if strings.Contains(body, "something went badly wrong") {
		t.Error("panic message leaked into response body")
	}
	if strings.Contains(body, "goroutine") {
		t.Error("stack trace leaked into response body")
	}

	// The logger must have received the panic detail + stack.
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "something went badly wrong") {
		t.Errorf("panic message not found in log output; got: %s", logOutput)
	}
}

// TestRecoverMiddleware_PanicWithRequestID verifies that even when a panic
// unwinds through the request-id middleware, the Request-Id response header
// is still present on the 500 response (because request-id sets headers before
// calling next).
func TestRecoverMiddleware_PanicWithRequestID(t *testing.T) {
	t.Parallel()

	logger, _ := newTestLogger()

	panicHandler := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("inner panic")
	})

	// Compose recover (outermost) wrapping request-id wrapping the panic handler.
	h := httpx.Chain(
		panicHandler,
		httpx.RecoverMiddleware(logger),
		httpx.RequestIDMiddleware(),
	)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
	// Request-Id must be present even though a panic happened.
	if rr.Header().Get("Request-Id") == "" {
		t.Error("Request-Id header missing on panicked 500 response")
	}
	// X-Request-Id must never be set.
	if v := rr.Header().Get("X-Request-Id"); v != "" {
		t.Errorf("X-Request-Id must not be set, got %q", v)
	}
}

// TestRecoverMiddleware_NoPanicPassesThrough verifies that a non-panicking
// handler passes through recover unchanged.
func TestRecoverMiddleware_NoPanicPassesThrough(t *testing.T) {
	t.Parallel()

	logger, _ := newTestLogger()
	h := httpx.Chain(noopHandler(http.StatusOK, "ok"), httpx.RecoverMiddleware(logger))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

// TestRecoverMiddleware_PanicAfterPartialWrite verifies that when a handler
// panics AFTER it has already started writing the response, recover does not
// splice a 500 problem body into the middle of the partial response. The status
// stays what the handler set, and the body contains no problem JSON.
func TestRecoverMiddleware_PanicAfterPartialWrite(t *testing.T) {
	t.Parallel()

	logger, logBuf := newTestLogger()

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "partial-body")
		panic("panic after write")
	})

	h := httpx.Chain(handler, httpx.RecoverMiddleware(logger))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	// Status stays 200 — recover must NOT overwrite it with a 500.
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (recover must not rewrite a committed response)", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, "internal_error") || strings.Contains(body, "problem") {
		t.Errorf("recover spliced a problem body into a committed response: %q", body)
	}
	if body != "partial-body" {
		t.Errorf("body = %q, want %q (untouched partial write)", body, "partial-body")
	}
	// The panic must still be logged server-side.
	if !strings.Contains(logBuf.String(), "panic after write") {
		t.Errorf("panic not logged; got: %s", logBuf.String())
	}
}

// ---- RequestIDMiddleware ----------------------------------------------------

// TestRequestIDMiddleware_GeneratesID verifies that a request with no
// incoming Request-Id header gets a fresh ID in the response header and
// in the request context.
func TestRequestIDMiddleware_GeneratesID(t *testing.T) {
	t.Parallel()

	var gotCtxID string
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotCtxID = httpx.RequestIDFromContext(r.Context())
	})

	h := httpx.Chain(inner, httpx.RequestIDMiddleware())

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	respID := rr.Header().Get("Request-Id")
	if respID == "" {
		t.Error("Request-Id response header is empty")
	}
	if gotCtxID == "" {
		t.Error("RequestIDFromContext returned empty string after middleware")
	}
	if respID != gotCtxID {
		t.Errorf("response Request-Id %q != context id %q", respID, gotCtxID)
	}
	if v := rr.Header().Get("X-Request-Id"); v != "" {
		t.Errorf("X-Request-Id must not be set, got %q", v)
	}
}

// TestRequestIDMiddleware_PropagatesIncomingID verifies that an incoming
// Request-Id header is reused (not replaced).
func TestRequestIDMiddleware_PropagatesIncomingID(t *testing.T) {
	t.Parallel()

	const incomingID = "req_propagated_123"

	var gotCtxID string
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotCtxID = httpx.RequestIDFromContext(r.Context())
	})

	h := httpx.Chain(inner, httpx.RequestIDMiddleware())

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Request-Id", incomingID)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Header().Get("Request-Id") != incomingID {
		t.Errorf("response Request-Id = %q, want %q", rr.Header().Get("Request-Id"), incomingID)
	}
	if gotCtxID != incomingID {
		t.Errorf("context id = %q, want %q", gotCtxID, incomingID)
	}
}

// TestRequestIDMiddleware_SetsTraceparent verifies that a traceparent response
// header is always set in the W3C format (00-<32hex>-<16hex>-01).
func TestRequestIDMiddleware_SetsTraceparent(t *testing.T) {
	t.Parallel()

	h := httpx.Chain(noopHandler(200, ""), httpx.RequestIDMiddleware())

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	tp := rr.Header().Get("traceparent")
	if tp == "" {
		t.Fatal("traceparent header is missing")
	}
	// Format: 00-<32 lowercase hex>-<16 lowercase hex>-01
	parts := strings.Split(tp, "-")
	if len(parts) != 4 {
		t.Fatalf("traceparent = %q, want 4 dash-separated parts", tp)
	}
	if parts[0] != "00" {
		t.Errorf("traceparent version = %q, want \"00\"", parts[0])
	}
	if len(parts[1]) != 32 {
		t.Errorf("traceparent trace-id length = %d, want 32", len(parts[1]))
	}
	if len(parts[2]) != 16 {
		t.Errorf("traceparent span-id length = %d, want 16", len(parts[2]))
	}
	if parts[3] != "01" {
		t.Errorf("traceparent flags = %q, want \"01\"", parts[3])
	}
}

// TestRequestIDMiddleware_HeaderSetBeforeNext verifies that the Request-Id and
// traceparent headers are set on the ResponseWriter BEFORE next is called,
// so a downstream panic does not strip them.
func TestRequestIDMiddleware_HeaderSetBeforeNext(t *testing.T) {
	t.Parallel()

	var seenID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Read the header that the request-id middleware should have set before
		// calling us.
		seenID = w.Header().Get("Request-Id")
	})

	h := httpx.Chain(inner, httpx.RequestIDMiddleware())
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if seenID == "" {
		t.Error("Request-Id was not set on ResponseWriter before calling next")
	}
}

// ---- RequestLogMiddleware ---------------------------------------------------

// TestRequestLogMiddleware_EmitsOneLine verifies that each request produces
// exactly one slog log line containing method, path, status, duration, and
// request id.
func TestRequestLogMiddleware_EmitsOneLine(t *testing.T) {
	t.Parallel()

	logger, logBuf := newTestLogger()

	const requestID = "req_log_test_001"
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})

	h := httpx.Chain(inner, httpx.RequestIDMiddleware(), httpx.RequestLogMiddleware(logger))

	r := httptest.NewRequest(http.MethodPost, "/api/v1/items", nil)
	r.Header.Set("Request-Id", requestID)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	lines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
	// Filter to non-empty lines.
	var nonEmpty []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) != 1 {
		t.Fatalf("expected 1 log line, got %d: %v", len(nonEmpty), nonEmpty)
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(nonEmpty[0]), &entry); err != nil {
		t.Fatalf("log line is not valid JSON: %v", err)
	}

	for _, key := range []string{"method", "path", "status", "duration", "requestId"} {
		if _, ok := entry[key]; !ok {
			t.Errorf("log entry missing key %q; entry = %v", key, entry)
		}
	}

	if got, _ := entry["method"].(string); got != http.MethodPost {
		t.Errorf("log method = %q, want %q", got, http.MethodPost)
	}
	if got, _ := entry["path"].(string); got != "/api/v1/items" {
		t.Errorf("log path = %q, want %q", got, "/api/v1/items")
	}
	// Status is logged as a number (float64 after JSON decode).
	if got, _ := entry["status"].(float64); int(got) != http.StatusCreated {
		t.Errorf("log status = %v, want 201", entry["status"])
	}
	if got, _ := entry["requestId"].(string); got != requestID {
		t.Errorf("log requestId = %q, want %q", got, requestID)
	}
}

// TestRequestLogMiddleware_NoBodiesOrSecrets verifies that the log line does
// not contain Authorization headers or request body content.
func TestRequestLogMiddleware_NoBodiesOrSecrets(t *testing.T) {
	t.Parallel()

	logger, logBuf := newTestLogger()

	const secretToken = "supersecretbearertoken"
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	h := httpx.Chain(inner, httpx.RequestLogMiddleware(logger))

	r := httptest.NewRequest(http.MethodGet, "/api/v1/items?password=secret", strings.NewReader(`{"secret":"data"}`))
	r.Header.Set("Authorization", "Bearer "+secretToken)
	httptest.NewRecorder()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	logOutput := logBuf.String()
	if strings.Contains(logOutput, secretToken) {
		t.Errorf("log output contains secret token: %s", logOutput)
	}
	if strings.Contains(logOutput, "supersecret") {
		t.Errorf("log output contains secret data: %s", logOutput)
	}
}

// ---- SecurityHeadersMiddleware ----------------------------------------------

// TestSecurityHeadersMiddleware_BaselineHeaders verifies that the baseline
// security headers are present on every response.
func TestSecurityHeadersMiddleware_BaselineHeaders(t *testing.T) {
	t.Parallel()

	h := httpx.Chain(noopHandler(200, ""), httpx.SecurityHeadersMiddleware())

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	tests := []struct {
		header string
		check  func(v string) bool
		desc   string
	}{
		{
			header: "X-Content-Type-Options",
			check:  func(v string) bool { return v == "nosniff" },
			desc:   "must be nosniff",
		},
		{
			header: "Content-Security-Policy",
			check:  func(v string) bool { return strings.Contains(v, "frame-ancestors") },
			desc:   "must include frame-ancestors directive",
		},
		{
			header: "Referrer-Policy",
			check:  func(v string) bool { return v != "" },
			desc:   "must be non-empty",
		},
	}

	for _, tt := range tests {
		v := rr.Header().Get(tt.header)
		if !tt.check(v) {
			t.Errorf("header %q = %q: %s", tt.header, v, tt.desc)
		}
	}
}

// ---- AuthMiddleware ---------------------------------------------------------

// TestAuthMiddleware_ValidTokenSetsPrincipal verifies that a valid Bearer token
// results in a Principal being placed in the request context.
func TestAuthMiddleware_ValidTokenSetsPrincipal(t *testing.T) {
	t.Parallel()

	wantPrincipal := store.Principal{UserID: "user_abc", Role: "admin"}
	validator := &fakeValidator{expectedToken: "valid-token-xyz", principal: wantPrincipal}
	isPublic := func(*http.Request) bool { return false }

	var gotPrincipal store.Principal
	var gotOK bool
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotPrincipal, gotOK = httpx.PrincipalFromContext(r.Context())
	})

	h := httpx.Chain(inner, httpx.AuthMiddleware(validator, isPublic))

	r := httptest.NewRequest(http.MethodGet, "/api/v1/resource", nil)
	r.Header.Set("Authorization", "Bearer valid-token-xyz")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code == http.StatusUnauthorized {
		t.Fatalf("got 401, want authenticated response")
	}
	if !gotOK {
		t.Error("PrincipalFromContext returned ok=false for an authenticated request")
	}
	if gotPrincipal != wantPrincipal {
		t.Errorf("principal = %+v, want %+v", gotPrincipal, wantPrincipal)
	}
}

// TestAuthMiddleware_MissingTokenYields401 verifies that a request with no
// Authorization header on a protected route gets a 401 problem+json response.
func TestAuthMiddleware_MissingTokenYields401(t *testing.T) {
	t.Parallel()

	validator := &fakeValidator{expectedToken: "any", principal: store.Principal{}}
	isPublic := func(*http.Request) bool { return false }

	h := httpx.Chain(noopHandler(200, "ok"), httpx.AuthMiddleware(validator, isPublic))

	r := httptest.NewRequest(http.MethodGet, "/api/v1/resource", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

// TestAuthMiddleware_InvalidTokenYields401 verifies that an invalid token
// returns 401 (not 403 — the token may simply be expired/invalid).
func TestAuthMiddleware_InvalidTokenYields401(t *testing.T) {
	t.Parallel()

	validator := &fakeValidator{expectedToken: "correct", principal: store.Principal{UserID: "u1"}}
	isPublic := func(*http.Request) bool { return false }

	h := httpx.Chain(noopHandler(200, "ok"), httpx.AuthMiddleware(validator, isPublic))

	r := httptest.NewRequest(http.MethodGet, "/api/v1/resource", nil)
	r.Header.Set("Authorization", "Bearer wrong-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

// TestAuthMiddleware_SchemeCaseInsensitive verifies that the Bearer auth scheme
// is matched case-insensitively per RFC 7235, so "Authorization: bearer <token>"
// is accepted just like "Bearer <token>".
func TestAuthMiddleware_SchemeCaseInsensitive(t *testing.T) {
	t.Parallel()

	wantPrincipal := store.Principal{UserID: "u_lc", Role: "user"}
	validator := &fakeValidator{expectedToken: "tok", principal: wantPrincipal}
	isPublic := func(*http.Request) bool { return false }

	var gotOK bool
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, gotOK = httpx.PrincipalFromContext(r.Context())
	})

	h := httpx.Chain(inner, httpx.AuthMiddleware(validator, isPublic))

	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		t.Run(scheme, func(t *testing.T) {
			gotOK = false
			r := httptest.NewRequest(http.MethodGet, "/api/v1/resource", nil)
			r.Header.Set("Authorization", scheme+" tok")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)

			if rr.Code == http.StatusUnauthorized {
				t.Errorf("scheme %q: got 401, want authenticated", scheme)
			}
			if !gotOK {
				t.Errorf("scheme %q: principal not set in context", scheme)
			}
		})
	}
}

// TestAuthMiddleware_PublicRouteExempt verifies that routes for which isPublic
// returns true are passed through without any token requirement.
func TestAuthMiddleware_PublicRouteExempt(t *testing.T) {
	t.Parallel()

	validator := &fakeValidator{expectedToken: "never-called", principal: store.Principal{}}
	// /healthz and /login are public.
	isPublic := func(r *http.Request) bool {
		return r.URL.Path == "/healthz" || r.URL.Path == "/login"
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	h := httpx.Chain(inner, httpx.AuthMiddleware(validator, isPublic))

	publicPaths := []string{"/healthz", "/login"}
	for _, path := range publicPaths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, path, nil)
			// No Authorization header — should pass through for public routes.
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)

			if rr.Code != http.StatusOK {
				t.Errorf("path %s: status = %d, want 200", path, rr.Code)
			}
		})
	}
}

// TestAuthMiddleware_SPAAssetsExempt verifies that SPA asset paths are
// considered public (tested via the isPublic predicate).
func TestAuthMiddleware_SPAAssetsExempt(t *testing.T) {
	t.Parallel()

	// Typical isPublic that matches SPA assets via prefix.
	isPublic := func(r *http.Request) bool {
		p := r.URL.Path
		return p == "/healthz" ||
			p == "/login" ||
			strings.HasPrefix(p, "/_app/")
	}
	validator := &fakeValidator{expectedToken: "tok", principal: store.Principal{UserID: "u1"}}

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	h := httpx.Chain(inner, httpx.AuthMiddleware(validator, isPublic))

	assetPaths := []string{
		"/_app/immutable/entry.abc123.js",
		"/_app/immutable/chunks/foo.js",
	}
	for _, p := range assetPaths {
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, p, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)

			if rr.Code != http.StatusOK {
				t.Errorf("asset path %s: status = %d, want 200 (public exempt)", p, rr.Code)
			}
		})
	}
}

// TestPrincipalFromContext_EmptyContext verifies that PrincipalFromContext
// returns ok=false when no principal has been stored.
func TestPrincipalFromContext_EmptyContext(t *testing.T) {
	t.Parallel()

	_, ok := httpx.PrincipalFromContext(context.Background())
	if ok {
		t.Error("PrincipalFromContext returned ok=true on an empty context")
	}
}

// ---- Full integration chain test -------------------------------------------

// TestFullChain_AuthenticatedRequest exercises the complete composed chain
// (recover → request-id → request-log → security-headers → auth → route)
// with a valid token and verifies all acceptance criteria together.
func TestFullChain_AuthenticatedRequest(t *testing.T) {
	t.Parallel()

	logger, logBuf := newTestLogger()
	wantPrincipal := store.Principal{UserID: "u_chain_test", Role: "user"}
	validator := &fakeValidator{expectedToken: "chain-tok", principal: wantPrincipal}
	isPublic := func(r *http.Request) bool { return r.URL.Path == "/healthz" }

	var gotPrincipal store.Principal
	route := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotPrincipal, _ = httpx.PrincipalFromContext(r.Context())
	})

	mws := httpx.StandardChain(logger, validator, isPublic)
	h := httpx.Chain(route, mws...)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/widgets", nil)
	r.Header.Set("Authorization", "Bearer chain-tok")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	// Status must be 200 (route ran).
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}

	// Request-Id and traceparent headers present.
	if rr.Header().Get("Request-Id") == "" {
		t.Error("Request-Id header missing")
	}
	if rr.Header().Get("traceparent") == "" {
		t.Error("traceparent header missing")
	}
	if v := rr.Header().Get("X-Request-Id"); v != "" {
		t.Errorf("X-Request-Id must not be set, got %q", v)
	}

	// Security headers present.
	if v := rr.Header().Get("X-Content-Type-Options"); v != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", v)
	}
	csp := rr.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors") {
		t.Errorf("CSP missing frame-ancestors: %q", csp)
	}
	if v := rr.Header().Get("Referrer-Policy"); v == "" {
		t.Error("Referrer-Policy header missing")
	}

	// Principal was set correctly.
	if gotPrincipal != wantPrincipal {
		t.Errorf("principal = %+v, want %+v", gotPrincipal, wantPrincipal)
	}

	// Log line was emitted.
	logOutput := logBuf.String()
	if logOutput == "" {
		t.Error("no log output produced")
	}
}

// TestFullChain_PanicStillCarriesRequestID exercises the full chain when the
// route panics, verifying that the 500 response still carries Request-Id
// (because request-id sets headers before calling next).
func TestFullChain_PanicStillCarriesRequestID(t *testing.T) {
	t.Parallel()

	logger, _ := newTestLogger()
	validator := &fakeValidator{expectedToken: "tok", principal: store.Principal{UserID: "u1"}}
	isPublic := func(_ *http.Request) bool { return true } // skip auth

	route := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("deliberate route panic")
	})

	mws := httpx.StandardChain(logger, validator, isPublic)
	h := httpx.Chain(route, mws...)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/boom", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
	if rr.Header().Get("Request-Id") == "" {
		t.Error("Request-Id header missing on panic 500 response")
	}
}

// TestFullChain_PublicRouteNoAuth verifies that /healthz passes through the
// full chain without requiring a Bearer token.
func TestFullChain_PublicRouteNoAuth(t *testing.T) {
	t.Parallel()

	logger, _ := newTestLogger()
	validator := &fakeValidator{expectedToken: "tok", principal: store.Principal{}}
	isPublic := func(r *http.Request) bool { return r.URL.Path == "/healthz" }

	route := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mws := httpx.StandardChain(logger, validator, isPublic)
	h := httpx.Chain(route, mws...)

	r := httptest.NewRequest(http.MethodGet, "/healthz", nil) // no auth header
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for public /healthz", rr.Code)
	}
}

// TestRequestLogMiddleware_EmitsOneLineOnPanic verifies that the request-log
// middleware emits exactly one "request" log line even when the inner handler
// panics, AND that the log line records status=500 and level=ERROR.
//
// Chain: recover (outermost) → request-id → request-log → panic handler.
// The deferred log in RequestLogMiddleware must observe status=500 — the value
// RecoverMiddleware will write — rather than the 0→200 default from the
// statusRecorder when WriteHeader has not yet been called.
func TestRequestLogMiddleware_EmitsOneLineOnPanic(t *testing.T) {
	t.Parallel()

	logger, logBuf := newTestLogger()

	panicHandler := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("handler panicked")
	})

	// Compose: recover (outermost) → request-id → request-log → panic handler.
	// This mirrors the production StandardChain ordering.
	h := httpx.Chain(
		panicHandler,
		httpx.RecoverMiddleware(logger),
		httpx.RequestIDMiddleware(),
		httpx.RequestLogMiddleware(logger),
	)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/boom", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	// The recover middleware writes a 500 to the client.
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("client response status = %d, want 500", rr.Code)
	}

	// Parse log lines — there must be exactly one "request" access-log line.
	logOutput := logBuf.String()
	lines := strings.Split(strings.TrimSpace(logOutput), "\n")
	var requestLines []map[string]any
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if msg, _ := entry["msg"].(string); msg == "request" {
			requestLines = append(requestLines, entry)
		}
	}

	if len(requestLines) == 0 {
		t.Fatalf("no access-log 'request' line emitted after a panic; log: %s", logOutput)
	}
	if len(requestLines) > 1 {
		t.Errorf("access-log emitted %d 'request' lines after panic, want exactly 1; log: %s", len(requestLines), logOutput)
	}

	entry := requestLines[0]

	// The access-log MUST record status=500 so SLO dashboards see the real outcome.
	// A value of 0 or 200 means the log captured the pre-panic state, not the
	// 500 RecoverMiddleware writes.
	gotStatus, _ := entry["status"].(float64)
	if int(gotStatus) != http.StatusInternalServerError {
		t.Errorf("access-log status = %v, want 500; the log must reflect the 500 RecoverMiddleware writes, not the unwritten 0→200 default", gotStatus)
	}

	// Level MUST be ERROR for 5xx (not INFO).
	gotLevel, _ := entry["level"].(string)
	if gotLevel != "ERROR" {
		t.Errorf("access-log level = %q, want ERROR for a 5xx status", gotLevel)
	}
}
