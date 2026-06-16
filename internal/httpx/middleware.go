package httpx

// This file implements the cross-cutting middleware chain:
//
//	recover → request-id → request-log → security-headers → auth → route
//
// Each middleware is a plain func(http.Handler) http.Handler (the Middleware
// type defined in router.go).  All dependencies are injected as parameters so
// the composition root can supply production vs test values without any
// package-level state.
//
// Public symbols:
//   - RecoverMiddleware(logger) Middleware
//   - RequestIDMiddleware() Middleware
//   - RequestLogMiddleware(logger) Middleware
//   - SecurityHeadersMiddleware() Middleware
//   - AuthMiddleware(validator, isPublic) Middleware
//   - TokenValidator interface
//   - ContextWithPrincipal / PrincipalFromContext
//   - StandardChain(logger, validator, isPublic) []Middleware

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/qovira/qovira/internal/store"
)

// Session cookie and CSRF constants. Exported so the auth handler (login) and
// client-side code generators can reference the same names without
// string-literal duplication.
const (
	// SessionCookieName is the __Host- prefixed cookie that carries the Qovira
	// session token. The __Host- prefix enforces Secure, no Domain, and Path=/.
	SessionCookieName = "__Host-qovira_session"

	// CSRFCookieName is the non-HttpOnly cookie whose value the browser-side
	// JavaScript reads and echoes as the CSRF-Token request header.
	CSRFCookieName = "qovira_csrf"

	// CSRFHeaderName is the request header that must match CSRFCookieName for
	// unsafe (non-GET) cookie-authenticated requests.
	CSRFHeaderName = "CSRF-Token"
)

// TokenValidator is the seam for token validation. The concrete implementation is provided by the Identity & Auth
// slice; tests inject a fake.
type TokenValidator interface {
	ValidateToken(ctx context.Context, token string) (store.Principal, error)
}

// ContextWithPrincipal returns a new context carrying the given Principal. The auth middleware stores the
// authenticated identity here; handlers and downstream layers retrieve it with PrincipalFromContext.
func ContextWithPrincipal(ctx context.Context, p store.Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// PrincipalFromContext retrieves the Principal stored by ContextWithPrincipal. ok is false when no Principal has been
// placed in the context (e.g. the route is public or auth was not run).
func PrincipalFromContext(ctx context.Context) (store.Principal, bool) {
	p, ok := ctx.Value(principalKey).(store.Principal)
	return p, ok
}

// StandardChain returns the ordered slice of cross-cutting middlewares for the
// standard request path. The slice is ordered outermost-first:
//
//	[0] recover — catches panics from any inner layer
//	[1] request-id — propagates or generates a correlation ID
//	[2] request-log — emits one structured log line per request
//	[3] security-headers — writes the baseline security response headers
//	[4] auth — validates the Bearer token and populates Principal in context
//
// Pass the returned slice directly to Chain so the first element is outermost:
//
//	h := Chain(myRoute, StandardChain(logger, v, isPublic)...)
func StandardChain(logger *slog.Logger, validator TokenValidator, isPublic func(*http.Request) bool) []Middleware {
	return []Middleware{
		RecoverMiddleware(logger),
		RequestIDMiddleware(),
		RequestLogMiddleware(logger),
		SecurityHeadersMiddleware(),
		AuthMiddleware(validator, isPublic),
	}
}

// RecoverMiddleware is the outermost wrapper. It catches any panic that escapes inner middleware or the route handler,
// logs the full detail and stack trace server-side via logger, and writes a generic 500 problem+json response to the
// client. The panic value and stack are never sent to the client.
//
// Because request-id runs INSIDE recover, the Request-Id response header may already be set on the ResponseWriter when
// the panic unwinds — this is intentional (see RequestIDMiddleware doc). If it is set, the 500 response carries it; if
// not (panic in recover itself, or recover is used without request-id), the header is absent but the body still
// carries requestId from context (or the unknownRequestID sentinel).
func RecoverMiddleware(logger *slog.Logger) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Wrap the writer so the deferred recovery can tell whether the response was already started. Writing a
			// problem body on top of a partially-sent response would corrupt it.
			guard := &responseGuard{ResponseWriter: w}
			defer func() {
				if v := recover(); v != nil {
					stack := debug.Stack()
					logger.Error("httpx: panic recovered",
						"panic", fmt.Sprintf("%v", v),
						"stack", string(stack),
						"method", r.Method,
						"path", r.URL.Path,
					)
					if guard.committed {
						// The handler already began writing before it panicked; the status line is on the wire and the
						// body is partial. We cannot turn it into a clean 500 — log and let the connection close with a
						// truncated response rather than splice a JSON problem into the middle of it.
						logger.Error("httpx: panic after response was partially written; cannot send error body",
							"method", r.Method,
							"path", r.URL.Path,
						)
						return
					}
					WriteProblem(guard, r, Problem{
						Title:  "Internal server error",
						Status: http.StatusInternalServerError,
						Detail: "An unexpected error occurred. Quote the requestId when contacting support.",
						Code:   "internal_error",
					})
				}
			}()
			next.ServeHTTP(guard, r)
		})
	}
}

// responseGuard wraps an http.ResponseWriter to record whether the response has been committed (a status line or any
// body byte written). RecoverMiddleware uses it to avoid writing an error body on top of a response a panicking handler
// had already begun.
type responseGuard struct {
	http.ResponseWriter
	committed bool
}

func (g *responseGuard) WriteHeader(code int) {
	g.committed = true
	g.ResponseWriter.WriteHeader(code)
}

func (g *responseGuard) Write(b []byte) (int, error) {
	g.committed = true
	return g.ResponseWriter.Write(b)
}

// RequestIDMiddleware propagates or generates a per-request correlation ID.
//
// If the incoming request carries a "Request-Id" header, that value is reused; otherwise a new random-hex ID is
// generated. The ID is stored in the request context via ContextWithRequestID so WriteProblem and the log middleware
// can read it.
//
// Crucially, both the "Request-Id" and "traceparent" response headers are set on the ResponseWriter BEFORE calling
// next.ServeHTTP. This ensures that even when a downstream panic unwinds back to RecoverMiddleware (which sits
// outermost), those headers are already present on the wire — the recovered 500 still carries them.
//
// Header names:
//   - "Request-Id" (plain, per the HTTP house guide — never "X-Request-Id")
//   - "traceparent" (W3C Trace Context: 00-<32hex>-<16hex>-01)
func RequestIDMiddleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("Request-Id")
			if id == "" {
				id = generateHex(16) // 128 bits → 32 hex chars
			}

			// Derive a W3C traceparent. Use a fresh random trace-id so every request appears as its own root trace
			// (until we have real OTel propagation). The span-id is also random.
			traceID := generateHex(16) // 128 bits → 32 hex
			spanID := generateHex(8)   // 64 bits → 16 hex
			traceparent := "00-" + traceID + "-" + spanID + "-01"

			// Set response headers BEFORE calling next so that if next panics, these headers are already committed to
			// the ResponseWriter header map (recover can then write the 500 body safely).
			w.Header().Set("Request-Id", id)
			w.Header().Set("traceparent", traceparent)

			// Store in context so downstream layers (logging, WriteProblem) can read the correlation ID without
			// touching response headers.
			ctx := ContextWithRequestID(r.Context(), id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// generateHex returns n random bytes encoded as a lowercase hex string (2n chars). It panics if the OS entropy source
// is unavailable — that is a fatal system error.
func generateHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("httpx: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// statusRecorder wraps an http.ResponseWriter to capture the first status code written via WriteHeader (or the implicit
// 200 on first Write). Used by RequestLogMiddleware so it can log the status after the handler returns.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// statusCode returns the recorded status or 200 if WriteHeader was never called.
func (r *statusRecorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// RequestLogMiddleware emits exactly one structured log line per request after
// the handler returns. The line contains:
//
//   - method  — HTTP verb
//   - path    — URL path (no query string, no headers, no body)
//   - status  — response HTTP status code
//   - duration — wall-clock duration in milliseconds
//   - requestId — correlation ID from context (set by RequestIDMiddleware)
//
// It intentionally omits query strings, request headers (including Authorization), response headers, and body content
// to avoid logging PII or secrets. The log level is Info for 1xx–4xx and Error for 5xx.
func RequestLogMiddleware(logger *slog.Logger) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}

			next.ServeHTTP(rec, r)

			duration := time.Since(start).Milliseconds()
			status := rec.statusCode()
			reqID := RequestIDFromContext(r.Context())

			level := slog.LevelInfo
			if status >= http.StatusInternalServerError {
				level = slog.LevelError
			}

			logger.LogAttrs(r.Context(), level, "request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int64("duration", duration),
				slog.String("requestId", reqID),
			)
		})
	}
}

// SecurityHeadersMiddleware sets the v0.1 baseline security response headers
// on every response:
//
//   - X-Content-Type-Options: nosniff — prevents MIME sniffing
//   - Content-Security-Policy: from spaCSP() — "default-src 'self'; script-src
//     'self' <hashes>; frame-ancestors 'none'", where <hashes> are the SHA-256
//     digests of the embedded SPA's first-party inline scripts (see spa_csp.go).
//     No 'unsafe-inline' is ever used; the policy is identical in every build.
//   - Referrer-Policy: strict-origin-when-cross-origin
//
// These are set before calling next so that handlers can override them for specific responses if needed (e.g. a
// download endpoint that needs a different frame policy). Full CSP hardening is deferred to the v0.2 security slice.
func SecurityHeadersMiddleware() Middleware {
	policy := spaCSP()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Content-Security-Policy", policy)
			w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
			next.ServeHTTP(w, r)
		})
	}
}

// SessionTokenFromRequest extracts the session token from r using the same
// precedence the auth middleware uses:
//  1. [SessionCookieName] cookie — the primary browser path.
//  2. Authorization: Bearer <token> header — the API / programmatic path.
//
// Returns the token string and viaCookie=true when the cookie was used.
// Returns an empty token and viaCookie=false when neither source is present.
// Exported so logout handlers can retrieve the current session token without
// duplicating the extraction logic.
func SessionTokenFromRequest(r *http.Request) (token string, viaCookie bool) {
	if t := sessionCookie(r); t != "" {
		return t, true
	}
	return bearerToken(r), false
}

// AuthMiddleware authenticates each non-public request and places the resolved
// [store.Principal] in the request context via [ContextWithPrincipal].
//
// Token extraction order (cookie wins when both are present):
//  1. Session cookie [SessionCookieName] — the primary browser path.
//  2. Authorization: Bearer <token> header — the API / programmatic path.
//
// CSRF double-submit (cookie-authenticated requests only):
// When the token was extracted via cookie AND the HTTP method is unsafe
// (POST, PATCH, or DELETE), the [CSRFHeaderName] request header must be
// present and must equal the [CSRFCookieName] cookie value (compared via
// [crypto/subtle.ConstantTimeCompare]).  A missing or mismatched CSRF header
// results in a 403 problem+json response with code "csrf_failed".  GET
// requests and Bearer-authenticated requests are exempt.
//
// Routes for which isPublic(r) returns true pass through without any token
// check; no Principal is placed in context for those routes.
//
// On missing or invalid token for a non-public route, the middleware writes a
// 401 problem+json response with code "unauthenticated" and does not call next.
func AuthMiddleware(validator TokenValidator, isPublic func(*http.Request) bool) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isPublic(r) {
				next.ServeHTTP(w, r)
				return
			}

			// Cookie-first extraction via the shared helper (also used by logout handlers).
			token, viaCookie := SessionTokenFromRequest(r)

			if token == "" {
				WriteProblem(w, r, Problem{
					Title:  "Authentication required",
					Status: http.StatusUnauthorized,
					Detail: "Provide a session cookie or an Authorization: Bearer <token> header.",
					Code:   "unauthenticated",
				})
				return
			}

			principal, err := validator.ValidateToken(r.Context(), token)
			if err != nil {
				WriteProblem(w, r, Problem{
					Title:  "Authentication required",
					Status: http.StatusUnauthorized,
					Detail: "The provided token is invalid or has expired.",
					Code:   "unauthenticated",
				})
				return
			}

			// CSRF double-submit check: required for cookie-authenticated unsafe
			// methods only. Bearer-authed requests and safe methods are exempt.
			if viaCookie && isUnsafeMethod(r.Method) {
				if !csrfValid(r) {
					WriteProblem(w, r, Problem{
						Title:  "CSRF validation failed",
						Status: http.StatusForbidden,
						Detail: "The CSRF-Token header is missing or does not match the session cookie.",
						Code:   "csrf_failed",
					})
					return
				}
			}

			ctx := ContextWithPrincipal(r.Context(), principal)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// sessionCookie returns the value of the [SessionCookieName] cookie, or an
// empty string when the cookie is absent or empty.
func sessionCookie(r *http.Request) string {
	c, err := r.Cookie(SessionCookieName)
	if err != nil || c.Value == "" {
		return ""
	}
	return c.Value
}

// isUnsafeMethod reports whether method requires CSRF protection. Only POST,
// PATCH, and DELETE are considered unsafe for this check (GET and HEAD are
// safe; PUT is uncommon in Qovira's PATCH-first API but would be unsafe too —
// extend if needed).
func isUnsafeMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// csrfValid returns true when the CSRF-Token request header matches the
// qovira_csrf cookie value via constant-time comparison. Returns false when
// either the header or the cookie is absent, or when they differ.
func csrfValid(r *http.Request) bool {
	headerVal := r.Header.Get(CSRFHeaderName)
	if headerVal == "" {
		return false
	}
	c, err := r.Cookie(CSRFCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	// Constant-time comparison to prevent timing attacks.
	return subtle.ConstantTimeCompare([]byte(headerVal), []byte(c.Value)) == 1
}

// bearerToken extracts the token value from an "Authorization: Bearer <token>" header. Returns an empty string if the
// header is absent, empty, or does not start with "Bearer ".
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	v := r.Header.Get("Authorization")
	if len(v) <= len(prefix) {
		return ""
	}
	// RFC 7235: the auth-scheme token is case-insensitive, so accept "bearer" and any other casing of the scheme
	// name.
	if !strings.EqualFold(v[:len(prefix)], prefix) {
		return ""
	}
	return v[len(prefix):]
}
