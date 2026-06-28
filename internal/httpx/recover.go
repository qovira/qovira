package httpx

import (
	"log/slog"
	"net/http"

	"github.com/qovira/qovira/internal/api/problem"
)

// NewRecoveryMiddleware returns a middleware that catches any panic from next and writes a generic 500
// response in the house RFC 9457 problem+json shape. The panic value is logged via logger together with
// the request ID (if any) and the request's method + path so the captured correlation ID points operators
// to the root cause in the access log.
//
// Design note: this is the whole-server backstop. It covers BOTH the non-API surface (SPA routes, spec /
// docs) and the /api/v1 operations — Huma v2 does NOT install its own request-handler recovery, so a panic
// inside an operation handler propagates up to this middleware. Writing the house problem+json body here
// (rather than a plain-text 500) keeps the uniform error contract every other error already follows.
//
// The wrapper ordering that satisfies acceptance criteria AC3:
//
//	request-ID → access-log → recovery → handler
//
// The access-log wrapper sits outside recovery so it observes the 500 that recovery writes. The
// request-ID wrapper sits outermost so both access-log and recovery can read the ID from context, and so
// the Request-Id response header is set before recovery writes its body.
func NewRecoveryMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}

			// http.ErrAbortHandler is the stdlib sentinel a handler panics with to abort the response
			// silently. Re-panic it so net/http's own serve loop handles the deliberate abort as designed,
			// rather than logging it as an error and writing a spurious 500.
			if v == http.ErrAbortHandler {
				panic(v)
			}

			id := RequestID(r.Context())

			logger.Error("recovered from panic",
				"requestId", id,
				"method", r.Method,
				"path", r.URL.Path,
				"panic", v,
			)

			// If the response has already been committed (a handler wrote headers or body before panicking,
			// e.g. a streaming/SSE handler mid-write), the status line is already on the wire and a clean
			// error body can no longer be sent. Logging above is the best we can do; do not double-write.
			if rw, ok := w.(interface{ Committed() bool }); ok && rw.Committed() {
				return
			}

			// Otherwise emit the house RFC 9457 problem+json 500 with the correlation ID. Per §6.3 the body
			// is generic — it never leaks the panic value or a stack trace.
			d := problem.Internal("An unexpected internal error occurred.")
			d.RequestID = id
			problem.WriteJSON(w, d)
		}()

		next.ServeHTTP(w, r)
	})
}
