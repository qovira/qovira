package httpx

import (
	"log/slog"
	"net/http"
	"time"
)

// healthPath is the well-known health-check URL. Container HEALTHCHECK polls (~every 30 s) would flood
// stdout at INFO level; logging them at DEBUG lets operators set level=info in production and suppress the
// noise while retaining observability when they debug at level=debug.
const healthPath = "/api/v1/health"

// statusRecorder wraps http.ResponseWriter to capture the status code written by the inner handler and
// whether the response has been committed. The status feeds the access log; the committed flag lets the
// recovery middleware avoid writing a 500 over a response a handler already started.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

// WriteHeader intercepts the status code before forwarding to the real ResponseWriter and records that the
// response has been committed (the status line is on the wire).
func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}

	r.ResponseWriter.WriteHeader(code)
}

// Write records the implicit 200 that net/http emits when a handler streams a body without calling
// WriteHeader, so Committed reports accurately even for handlers that never set a status explicitly.
func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}

	return r.ResponseWriter.Write(b)
}

// Committed reports whether the response has been started (headers or body written). The recovery
// middleware uses it to skip writing a 500 over an already-committed response (e.g. a streaming handler
// that wrote part of a 200 before panicking).
func (r *statusRecorder) Committed() bool {
	return r.wroteHeader
}

// Unwrap returns the wrapped ResponseWriter so http.ResponseController can reach the underlying writer's
// optional behaviours (Flush, Hijack, SetReadDeadline, …). Without it, wrapping silently disables those for
// any handler below this middleware — e.g. a future SSE/streaming endpoint's Flush would fail. We deliberately
// do NOT forward io.ReaderFrom: the SPA is served from an embed.FS with no file descriptor, so there is no
// sendfile fast-path to preserve; revisit only if a real-filesystem handler is added below the chain.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// written returns the captured status code, defaulting to 200 when the response was never committed (the
// Go net/http convention: an implicit 200 when the handler writes a body without calling WriteHeader).
func (r *statusRecorder) written() int {
	if !r.wroteHeader {
		return http.StatusOK
	}

	return r.status
}

// NewAccessLogMiddleware returns a middleware that emits one structured slog line per request. Each line
// carries:
//   - method — HTTP method
//   - path   — URL path
//   - status — HTTP status code as written by the inner handler
//   - duration — wall-clock request duration as a float64 seconds value
//   - requestId — the correlation token from context (set by [NewRequestIDMiddleware])
//
// Logging level:
//   - /api/v1/health → slog.LevelDebug (container HEALTHCHECK polls must not flood stdout at INFO)
//   - all other paths → slog.LevelInfo
//
// This middleware must sit *outside* the recovery middleware in the chain so that it observes the 500
// status that recovery writes after a panic (acceptance criterion AC3).
func NewAccessLogMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		duration := time.Since(start).Seconds()
		status := rec.written()
		id := RequestID(r.Context())

		level := slog.LevelInfo
		if r.URL.Path == healthPath {
			level = slog.LevelDebug
		}

		logger.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration", duration,
			"requestId", id,
		)
	})
}
