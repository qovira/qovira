package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/qovira/qovira/internal/events"
)

// NewServer constructs an *http.Server ready to listen on addr. It builds the
// full request handler (health check, API base, per-user SSE stream, SPA) and
// sets sane timeouts. addr, version, and bus are plain values — the composition
// root passes its config values here; this package does not import
// internal/config or internal/cli.
//
// The /events route is a real per-user SSE stream authenticated by the
// principal placed in context by the auth middleware. It is intentionally NOT
// public — the composition root must exclude /events from the isPublic
// predicate passed to AuthMiddleware.
//
// NewServer does NOT start the listener or implement graceful shutdown. The
// serve command owns the lifecycle.
func NewServer(addr, version string, bus events.Bus) *http.Server {
	mux := buildMux(version, bus)

	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// buildMux constructs the ServeMux that routes all traffic handled by this
// server. Route precedence (most-specific first):
//
//  1. GET /healthz — unauthenticated health check, no middleware.
//  2. /api/v1/{path} — API catch-all; unknown paths return a JSON 404 problem.
//     Real endpoints are registered here by later slices.
//  3. /events — per-user SSE stream; bare all-methods pattern so the SPA
//     fallback cannot intercept it. Method and auth guards are in the handler.
//  4. / (everything else) — embedded SPA with SPA-fallback to index.html.
func buildMux(version string, bus events.Bus) *http.ServeMux {
	mux := http.NewServeMux()

	// 1. Health check — unauthenticated, no middleware.
	mux.HandleFunc("GET /healthz", healthzHandler(version))

	// 2. API catch-all — unknown /api/v1/... paths return a JSON 404 problem.
	// The trailing {path} wildcard matches any sub-path. Real endpoints will
	// be registered as more-specific patterns and take priority.
	mux.Handle("/api/v1/{path...}", Chain(
		http.HandlerFunc(apiNotFoundHandler),
	))

	// 3. SSE stream — bare all-methods pattern (no "GET " prefix) so the mux
	// always matches /events before the SPA fallback for any verb. The handler
	// enforces GET and requires an authenticated principal in context.
	mux.HandleFunc("/events", eventsHandler(bus))

	// 4. SPA fallback — serves the embedded SvelteKit build for all remaining
	// paths, with SPA fallback to index.html for unknown routes.
	mux.Handle("/", spaHandler())

	return mux
}

// healthzHandler returns a handler for GET /healthz. It is unauthenticated and
// returns a JSON object with status:"ok" and the server version. Feeds the
// Docker HEALTHCHECK instruction.
func healthzHandler(version string) http.HandlerFunc {
	type response struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	body, err := json.Marshal(response{Status: "ok", Version: version})
	if err != nil {
		// This cannot fail for a well-typed struct; log and use a static fallback.
		slog.Error("httpx: failed to marshal healthz body", "err", err)
		body = []byte(`{"status":"ok","version":"unknown"}`)
	}

	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(body); err != nil {
			slog.Error("httpx: failed to write healthz body", "err", err)
		}
	}
}

// apiNotFoundHandler is the catch-all for unknown /api/v1/... paths. It
// returns a RFC 9457 problem+json 404 response. HTML is never returned for
// API paths.
func apiNotFoundHandler(w http.ResponseWriter, r *http.Request) {
	WriteProblem(w, r, Problem{
		Title:  "Resource not found",
		Status: http.StatusNotFound,
		Detail: "The requested API resource does not exist.",
		Code:   "not_found",
	})
}
