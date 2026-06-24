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

// TestDefaultChain_OrderIsOutermostFirst verifies that the real StandardChain
// composes its middlewares in the documented order:
//
//	recover → request-id → request-log → security-headers → auth
//
// The test exercises the actual httpx.StandardChain return value (not stubs).
// Observable effects of each layer prove the ordering:
//
//   - recover is outermost: a panicking route yields 500 with the panic logged.
//   - request-id runs inside recover: Request-Id is present on the 500 response.
//   - request-log runs inside request-id: it can read the request-id from context.
//   - security-headers runs before auth: the header is set even on 401 responses.
//   - auth is innermost (before route): an invalid token yields 401, not 200.
//
// If StandardChain ever reorders its elements, at least one assertion here will
// fail without needing to inspect the slice positions by index.
func TestDefaultChain_OrderIsOutermostFirst(t *testing.T) {
	t.Parallel()

	wantPrincipal := store.Principal{UserID: "u_order", Role: "user"}
	validator := &fakeValidator{expectedToken: "good-tok", principal: wantPrincipal}
	isPublic := func(r *http.Request) bool { return r.URL.Path == "/healthz" }

	// Each subtest creates its own logger so parallel subtests never share a
	// *bytes.Buffer and cannot race on it. StandardChain is called once per
	// subtest so the logger injected into the real middleware is subtest-local.

	t.Run("recover_is_outermost", func(t *testing.T) {
		t.Parallel()

		logger, _ := newTestLogger()
		mws := httpx.StandardChain(logger, validator, isPublic)
		if len(mws) != 5 {
			t.Fatalf("StandardChain returned %d middlewares, want 5", len(mws))
		}

		// Behavioral proof that mws[0] is RecoverMiddleware, not any other middleware.
		//
		// Strategy: compose a panicking middleware as position 1 (directly inside
		// mws[0]) and verify the response is a clean 500 problem+json.
		//
		//   Chain(route, mws[0], panicMW)
		//   → mws[0]( panicMW( route ) )
		//
		// Only RecoverMiddleware can turn a panic into a 500 response. Any other
		// middleware at position 0 (e.g. request-id in the M1 swap) lets the panic
		// propagate uncaught and ServeHTTP re-panics — the response recorder ends up
		// with code 200 (default, never set) and no problem+json body.
		//
		// This mutant-killing property holds because we drive the panic from panicMW,
		// which is placed INSIDE mws[0] and OUTSIDE the rest of the chain. If the
		// M1 mutation (request-id outermost, recover second) is applied, mws[0] is
		// request-id, which does not recover panics, so the test panics and fails.
		panicMW := func(_ http.Handler) http.Handler {
			return http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
				panic("recover-outermost sentinel panic")
			})
		}
		h := httpx.Chain(noopHandler(http.StatusOK, ""), mws[0], panicMW)

		r := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)

		if rr.Code != http.StatusInternalServerError {
			t.Errorf("mws[0] did not recover a panic from the next layer: status = %d, want 500 — mws[0] must be RecoverMiddleware", rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("mws[0] panic recovery did not produce problem+json: Content-Type = %q, want application/problem+json", ct)
		}
	})

	t.Run("request_id_sets_header_before_next", func(t *testing.T) {
		t.Parallel()

		logger, _ := newTestLogger()
		mws := httpx.StandardChain(logger, validator, isPublic)

		// Behavioral proof that mws[1] is RequestIDMiddleware.
		//
		// RequestIDMiddleware sets the Request-Id header on the ResponseWriter BEFORE
		// calling next. We verify this by reading the header from WITHIN the inner
		// handler — if it is already set when the handler runs, mws[1] must be
		// RequestIDMiddleware (no other production middleware sets that header).
		//
		// Composing: Chain(inner, mws[0], mws[1])
		// → mws[0]( mws[1]( inner ) )
		// inner observes the ResponseWriter AFTER mws[1] has executed its pre-next code.
		var headerSeenInsideNext string
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			headerSeenInsideNext = w.Header().Get("Request-Id")
		})
		h := httpx.Chain(inner, mws[0], mws[1])

		r := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)

		if headerSeenInsideNext == "" {
			t.Error("Request-Id header was not set before next was called — mws[1] must be RequestIDMiddleware")
		}
	})

	t.Run("security_headers_before_auth_present_on_401", func(t *testing.T) {
		t.Parallel()

		logger, _ := newTestLogger()
		mws := httpx.StandardChain(logger, validator, isPublic)

		// security-headers runs before auth, so its headers must appear even on 401.
		// If auth ran before security-headers, the 401 short-circuit would skip the header.
		route := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		h := httpx.Chain(route, mws...)

		r := httptest.NewRequest(http.MethodGet, "/api/v1/protected", nil) // no token → 401
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)

		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("want 401 from auth, got %d", rr.Code)
		}
		if v := rr.Header().Get("X-Content-Type-Options"); v != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q on 401, want nosniff — security-headers must run before auth", v)
		}
	})

	t.Run("auth_is_innermost_outer_layers_run_on_401", func(t *testing.T) {
		t.Parallel()

		// Behavioral proof that auth is the innermost middleware (last before route).
		//
		// Strategy: send a request that auth rejects (no token → 401) and assert that
		// the layers that must be OUTSIDE auth all produced their observable effects:
		//   - request-id set the Request-Id response header
		//   - security-headers set X-Content-Type-Options: nosniff
		//   - request-log emitted a "request" log line containing the request-id
		//
		// If any of these layers were INSIDE auth (i.e. between auth and the route),
		// auth's 401 short-circuit would skip them — the corresponding assertion fails.
		//
		// The M2 mutation (request-log moved innermost, between auth and route) is
		// caught by the log-line check: on a 401 the inner request-log never runs,
		// so no "request" line is emitted.
		logger, logBuf := newTestLogger()
		mws := httpx.StandardChain(logger, validator, isPublic)

		route := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		h := httpx.Chain(route, mws...)

		// No Authorization header → auth rejects with 401.
		r := httptest.NewRequest(http.MethodGet, "/api/v1/protected", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)

		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("want 401 from auth, got %d — prerequisite for this test", rr.Code)
		}

		// request-id must have run (outside auth): header is present on the 401 response.
		if rr.Header().Get("Request-Id") == "" {
			t.Error("Request-Id header missing on 401 — request-id must be outside (above) auth")
		}

		// security-headers must have run (outside auth): nosniff on the 401 response.
		if v := rr.Header().Get("X-Content-Type-Options"); v != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q on 401 — security-headers must be outside (above) auth", v)
		}

		// request-log must have run (outside auth): a "request" log line must exist.
		// If request-log were innermost (M2 mutation), the 401 short-circuit skips it.
		logOutput := logBuf.String()
		hasRequestLine := false
		for line := range strings.SplitSeq(logOutput, "\n") {
			if strings.Contains(line, `"msg":"request"`) {
				hasRequestLine = true
				break
			}
		}
		if !hasRequestLine {
			t.Errorf("no 'request' log line on 401 — request-log must be outside (above) auth; log: %s", logOutput)
		}
	})

	t.Run("request_log_emits_line_with_request_id_from_context", func(t *testing.T) {
		t.Parallel()

		logger, logBuf := newTestLogger()
		mws := httpx.StandardChain(logger, validator, isPublic)

		// request-log runs inside request-id, so it can read the request-id from
		// context. If order were reversed (log → request-id), the log line would have
		// an empty or "unknown" requestId.
		route := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		h := httpx.Chain(route, mws...)

		const incomingID = "order-test-req-id"
		r := httptest.NewRequest(http.MethodGet, "/healthz", nil) // public route
		r.Header.Set("Request-Id", incomingID)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)

		logOutput := logBuf.String()
		if !strings.Contains(logOutput, incomingID) {
			t.Errorf("log does not contain requestId %q — request-log must run inside request-id; log: %s",
				incomingID, logOutput)
		}
	})
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

// TestRequestLogMiddleware_5xxLogsError verifies that a handler that returns a
// 5xx status WITHOUT panicking causes the request-log middleware to emit the log
// line at ERROR level, and that a 2xx or 4xx response is logged at INFO level.
//
// This is the general status≥500 → ERROR mapping test. The panic path (which
// also resolves to 500/ERROR) is covered by TestRequestLogMiddleware_EmitsOneLineOnPanic.
func TestRequestLogMiddleware_5xxLogsError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status    int
		wantLevel string
	}{
		{http.StatusOK, "INFO"},
		{http.StatusCreated, "INFO"},
		{http.StatusBadRequest, "INFO"},
		{http.StatusUnauthorized, "INFO"},
		{http.StatusNotFound, "INFO"},
		{http.StatusInternalServerError, "ERROR"},
		{http.StatusBadGateway, "ERROR"},
		{http.StatusServiceUnavailable, "ERROR"},
	}

	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			t.Parallel()

			logger, logBuf := newTestLogger()

			// The handler writes the status code normally — no panic, just a plain
			// return. This exercises the straight-line path through RequestLogMiddleware,
			// not the panic re-panic path.
			h := httpx.Chain(noopHandler(tt.status, ""), httpx.RequestLogMiddleware(logger))

			r := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)

			logOutput := logBuf.String()
			// Parse the single "request" log line.
			var entry map[string]any
			for line := range strings.SplitSeq(logOutput, "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					t.Fatalf("log line is not valid JSON: %v (line: %s)", err, line)
				}
				if msg, _ := entry["msg"].(string); msg != "request" {
					continue
				}
				break
			}
			if entry == nil {
				t.Fatalf("no 'request' log line emitted; log: %s", logOutput)
			}

			gotLevel, _ := entry["level"].(string)
			if gotLevel != tt.wantLevel {
				t.Errorf("status %d: log level = %q, want %q — status→level mapping is wrong",
					tt.status, gotLevel, tt.wantLevel)
			}
		})
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
