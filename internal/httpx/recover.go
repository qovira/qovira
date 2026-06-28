package httpx

import (
	"fmt"
	"log/slog"
	"net/http"
)

// NewRecoveryMiddleware returns a middleware that catches any panic from next and writes a generic 500
// response. The panic value is logged (via slog.Default()) with the request ID (if any) and the request's
// method + path so that the captured correlation ID points operators to the root cause in the access log.
//
// Design note: this is the whole-server backstop covering the non-API surface (SPA routes, spec/docs).
// Huma installs its own recovery for /api/v1 operations; this middleware does NOT double-handle those —
// it simply provides a safety net in case Huma's own recovery is absent or misconfigured.
//
// The wrapper ordering that satisfies acceptance criteria AC3:
//
//	request-ID → access-log → recovery → handler
//
// The access-log wrapper sits outside recovery so it observes the 500 that recovery writes. The
// request-ID wrapper sits outermost so both access-log and recovery can read the ID from context.
func NewRecoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				// Log the panic with correlation context. Use slog.Default() rather than requiring a
				// logger parameter to keep the signature simple — the request-ID middleware stores the
				// ID in context where we can read it.
				slog.Default().Error("recovered from panic",
					"requestId", RequestID(r.Context()),
					"method", r.Method,
					"path", r.URL.Path,
					"panic", fmt.Sprintf("%v", v),
				)

				// Per §6.3: 5xx bodies are generic — never leak the panic value or a stack trace.
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()

		next.ServeHTTP(w, r)
	})
}
