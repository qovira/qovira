package httpx

import (
	"net/http"
	"strings"
)

// CORS policy hard-coded constants. These define the server's default cross-origin surface. Origin-based
// allow-listing is in [CORSConfig].
//
// TODO(config): promote AllowedMethods and AllowedHeaders to CORSConfig fields (or merge into the
// instance config model, unit 9) when fine-grained per-deployment control is needed.
const (
	// corsAllowedMethods lists the HTTP methods permitted in cross-origin requests. These are the safe
	// verbs for a standard REST API surface; CONNECT and TRACE are intentionally excluded.
	corsAllowedMethods = "GET, POST, PUT, PATCH, DELETE, OPTIONS"

	// corsAllowedHeaders lists the request headers cross-origin clients may send. Authorization and
	// Content-Type are required for bearer-token APIs; Idempotency-Key is required per the HTTP guide's
	// §idempotency rule; Request-Id allows clients to supply their own correlation token.
	corsAllowedHeaders = "Authorization, Content-Type, Idempotency-Key, Request-Id"

	// corsExposeHeaders lists the response headers that browsers are allowed to read from cross-origin
	// responses. Request-Id is the correlation token browsers need to relate responses to access-log lines.
	corsExposeHeaders = "Request-Id"

	// corsMaxAge is the number of seconds a preflight response may be cached by the browser. 600 seconds
	// (10 minutes) reduces preflight round-trips without letting stale policy linger too long.
	corsMaxAge = "600"
)

// CORSConfig holds the allow-listed origins for the CORS middleware. All other policy parameters
// (methods, headers, max-age) are hard-coded constants that match the Qovira API surface.
//
// TODO(config): wire AllowedOrigins from the instance config model (unit 9) so operators can set their
// deployment's allowed origins without recompiling.
type CORSConfig struct {
	// AllowedOrigins is the list of origins that are permitted to make cross-origin requests. An empty
	// or nil slice means same-origin-default (no cross-origin access granted). Each entry must be an
	// exact match including scheme and host (e.g. "https://app.qovira.ai").
	AllowedOrigins []string
}

// NewCORSMiddleware returns a middleware that enforces the CORS policy described by cfg:
//
//   - An OPTIONS preflight from an allow-listed origin returns 204 with the required CORS headers and
//     short-circuits the inner handler.
//   - A simple or non-preflight cross-origin request from an allow-listed origin receives
//     Access-Control-Allow-Origin set to that exact origin.
//   - A request from a non-allow-listed origin (or with no Origin header) receives no CORS headers;
//     the browser will block the response, which is the intended same-origin-default behaviour.
//   - Access-Control-Allow-Origin is NEVER set to "*" and is NEVER paired with
//     Access-Control-Allow-Credentials: true (both would violate the HTTP guide §11.4).
func NewCORSMiddleware(cfg CORSConfig, next http.Handler) http.Handler {
	// Build the allow-set for O(1) lookup on each request.
	allowed := make(map[string]struct{}, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		allowed[o] = struct{}{}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// No Origin header — same-origin browser request or server-to-server call; pass through.
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}

		_, permitted := allowed[origin]

		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			// Preflight request: always short-circuit, set headers only when permitted.
			if permitted {
				setCORSHeaders(w, origin)
				w.Header().Set("Access-Control-Allow-Methods", corsAllowedMethods)
				w.Header().Set("Access-Control-Allow-Headers", corsAllowedHeaders)
				w.Header().Set("Access-Control-Max-Age", corsMaxAge)
			}

			w.WriteHeader(http.StatusNoContent)

			return
		}

		// Simple or actual cross-origin request.
		if permitted {
			setCORSHeaders(w, origin)
		}

		next.ServeHTTP(w, r)
	})
}

// setCORSHeaders sets the common CORS response headers shared by preflight and simple requests.
// It deliberately never sets Access-Control-Allow-Credentials: true alongside a wildcard origin — the
// only way this function is reachable is via the explicit allow-list check above (no reflection).
func setCORSHeaders(w http.ResponseWriter, origin string) {
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Expose-Headers", corsExposeHeaders)
	// Vary tells caches that the response differs by Origin, preventing one client's response from being
	// served to a different origin.
	vary := w.Header().Get("Vary")
	if vary == "" {
		w.Header().Set("Vary", "Origin")
	} else if !strings.Contains(vary, "Origin") {
		w.Header().Set("Vary", vary+", Origin")
	}
}
