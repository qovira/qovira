package httpx

import (
	"net/http"
	"slices"
)

// Middleware is a function that wraps an http.Handler to add behaviour
// (logging, auth, tracing, etc.) before or after calling the next handler.
// Middleware functions are composed via Chain.
type Middleware func(http.Handler) http.Handler

// Chain wraps h in each middleware from left to right, making the first middleware the outermost wrapper (first to run
// on the way in, last on the way out). With no middleware, Chain returns h unchanged.
//
// Usage:
//
//	handler := Chain(myHandler, requestID, logging, auth)
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	// Apply in reverse so the first element ends up outermost.
	for _, v := range slices.Backward(mws) {
		h = v(h)
	}
	return h
}

// Router wraps a *http.ServeMux and is the mount point for module routes. Each module receives a *Router in its Routes
// method and registers its own fully-qualified patterns, by convention under /api/v1/... (e.g. "GET
// /api/v1/reminders"). Go's ServeMux resolves by specificity, so a concrete module pattern like "GET /api/v1/reminders"
// always beats the "/api/v1/{path...}" catch-all registered by NewServer, regardless of registration order.
//
// NewServer registers its own fixed routes (healthz, API catch-all, /events, /) after module routes are mounted; both
// sets share the same underlying mux.
type Router struct {
	mux *http.ServeMux
}

// NewRouter constructs and returns a ready-to-use *Router backed by a fresh *http.ServeMux.
func NewRouter() *Router {
	return &Router{mux: http.NewServeMux()}
}

// Handle registers the handler for the given pattern on the underlying mux.
// Pattern syntax follows [net/http.ServeMux].
func (r *Router) Handle(pattern string, h http.Handler) {
	r.mux.Handle(pattern, h)
}

// HandleFunc registers the handler function for the given pattern on the
// underlying mux. Pattern syntax follows [net/http.ServeMux].
func (r *Router) HandleFunc(pattern string, h http.HandlerFunc) {
	r.mux.HandleFunc(pattern, h)
}
