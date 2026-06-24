package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/qovira/qovira/internal/events"
)

// NewServer constructs an *http.Server ready to listen on addr. It mounts the fixed system routes onto router's mux
// (health check, API base catch-all, per-user SSE stream, SPA fallback), then wraps the mux with the supplied
// middleware chain, and sets sane timeouts.
//
// Module routes must be mounted on router before NewServer is called; they share the same underlying mux and take
// priority over the catch-all because Go's ServeMux resolves by specificity regardless of registration order.
//
// addr, version, and bus are plain values; this package does not import internal/config or internal/cli. NewServer does
// NOT start the listener or implement graceful shutdown — the serve command owns the lifecycle.
//
// The /events route is a real per-user SSE stream authenticated by the principal placed in context by the auth
// middleware. The composition root's isPublic predicate must return false for /events.
func NewServer(addr, version string, router *Router, bus events.Bus, mws ...Middleware) *http.Server {
	mountFixedRoutes(router, version, bus)

	return &http.Server{
		Addr:              addr,
		Handler:           Chain(router.mux, mws...),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// mountFixedRoutes registers the system-owned routes onto router. It is called by NewServer after all module routes are
// already mounted, so module patterns take natural precedence over the catch-all routes below.
//
// Route precedence (most-specific first, per ServeMux):
//
//  1. GET /healthz — unauthenticated health check.
//  2. /api/v1/{path...} — API catch-all; unknown paths return a JSON 404 problem. Real module endpoints registered
//     earlier take priority.
//  3. /events — per-user SSE stream; bare all-methods pattern so the SPA fallback cannot intercept it.
//  4. / (everything else) — embedded SPA with SPA-fallback to index.html.
func mountFixedRoutes(router *Router, version string, bus events.Bus) {
	// 1. Health check — the middleware chain (recover/request-id/log/security-headers) wraps this route too, but the
	//    isPublic predicate exempts it from auth. Adding observability middleware here is intentional.
	router.mux.HandleFunc("GET /healthz", healthzHandler(version))

	// 2. API catch-all — unknown /api/v1/... paths return a JSON 404 problem. Module routes registered before this call
	// take priority due to specificity. The bare "/api/v1" route is registered explicitly because the {path...}
	// wildcard requires at least one path segment; without it Go's ServeMux redirects GET /api/v1 → /api/v1/ (307),
	// which returns HTML via the SPA fallback. Registering "/api/v1" directly prevents that redirect.
	router.mux.HandleFunc("/api/v1", apiNotFoundHandler)
	router.mux.HandleFunc("/api/v1/{path...}", apiNotFoundHandler)

	// 3. SSE stream — bare all-methods pattern (no "GET " prefix) so the mux always matches /events before the SPA
	//    fallback for any verb. The handler enforces GET and requires an authenticated principal in context.
	router.mux.HandleFunc("/events", eventsHandler(bus))

	// 4. SPA fallback — serves the embedded SvelteKit build for all remaining paths, with SPA fallback to index.html
	// for unknown routes.
	router.mux.Handle("/", spaHandler())
}

// healthzHandler returns a handler for GET /healthz. It is unauthenticated and returns a JSON object with status:"ok"
// and the server version. Feeds the Docker HEALTHCHECK instruction.
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

// apiNotFoundHandler is the catch-all for unknown /api/v1/... paths. It returns a RFC 9457 problem+json 404 response.
// HTML is never returned for API paths.
func apiNotFoundHandler(w http.ResponseWriter, r *http.Request) {
	WriteProblem(w, r, Problem{
		Title:  "Resource not found",
		Status: http.StatusNotFound,
		Detail: "The requested API resource does not exist.",
		Code:   "not_found",
	})
}
