package httpx

import "net/http"

// Middleware is a function that wraps an http.Handler to add behaviour
// (logging, auth, tracing, etc.) before or after calling the next handler.
// Middleware functions are composed via Chain.
type Middleware func(http.Handler) http.Handler

// Chain wraps h in each middleware from left to right, making the first
// middleware the outermost wrapper (first to run on the way in, last on the
// way out). With no middleware, Chain returns h unchanged.
//
// Usage:
//
//	handler := Chain(myHandler, requestID, logging, auth)
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	// Apply in reverse so the first element ends up outermost.
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
