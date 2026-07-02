package httpx

import "net/http"

// NewSecurityHeadersMiddleware returns a middleware that sets baseline browser-security response headers on
// every response — including SPA assets, API JSON, SSE streams, recovery 500s, and CORS preflights — before
// delegating to next.
//
// Headers set now:
//
//   - X-Content-Type-Options: nosniff — prevents browsers from MIME-sniffing a response away from the
//     declared Content-Type. This closes the nosniff gap noted in the app-layer TODO(security) and applies
//     to every response regardless of content type or status code.
//
// TODO(security): add Content-Security-Policy and X-Frame-Options (or frame-ancestors CSP directive) before
// a real web client ships. Both require knowing the deployment's trusted origins, so they are deferred to
// the pre-web-client hardening pass (unit 9 / instance config wiring).
func NewSecurityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}
