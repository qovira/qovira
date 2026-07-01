// Package api builds the Qovira API surface: the Huma instance mounted under /api/v1 and its operations, on
// the caller's mux (shared with the SPA). Every error — Huma's 422/415/500 and the routing-level 404/405
// from newAPIFallback — emerges as application/problem+json in the house RFC 9457 shape. Shaping is
// centralized (see internal/api/problem and requestIDTransformer), so handlers do no per-error work.
// Most-specific-pattern routing keeps /api/v1/... with Huma and lets everything else fall through to the SPA
// catch-all, so no prefix-stripping is needed.
package api

import (
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/qovira/qovira/internal/api/problem"
	"github.com/qovira/qovira/internal/buildinfo"
	"github.com/qovira/qovira/internal/httpx"
)

// maxBodyBytes is the default request-body ceiling for every registered operation. It aliases
// [httpx.MaxBodyBytes] so the per-operation cap and the server-edge backstop (http.MaxBytesHandler) stay in
// lockstep. An operation may override MaxBodyBytes as its contract demands.
const maxBodyBytes = httpx.MaxBodyBytes

// New builds the Huma API mounted on the caller's mux under the /api/v1 prefix, registers all operations,
// installs the /api/ fallback for routing-level 404/405, and returns the huma.API for callers that need to
// introspect or extend it. The log parameter is reserved for future middleware and is accepted now so the
// signature is stable across that addition without requiring a composition-root change.
func New(mux *http.ServeMux, bi buildinfo.Info, _ *slog.Logger) huma.API {
	cfg := huma.DefaultConfig("Qovira", bi.Version)

	// Append the requestID transformer AFTER DefaultConfig's link transformer so that:
	//   1. The link transformer runs first and may add $schema to non-error bodies (success responses).
	//   2. Our transformer sees the final body; for *problem.Details it injects requestId and returns
	//      early, preventing the link transformer's $schema from ever touching error bodies.
	cfg.Transformers = append(cfg.Transformers, requestIDTransformer)

	// TODO(config): docs UI enable/disable toggle — to disable the Stoplight Elements docs page (e.g. in
	// production hardening, unit 9), set cfg.DocsPath = "" here before calling humago.NewWithPrefix. The
	// route is intentionally left public for now; the CSP carve-out for /docs is deferred to unit 6.

	ha := humago.NewWithPrefix(mux, "/api/v1", cfg)

	registerMeta(ha, bi)

	// Register the fallback AFTER Huma operations so most-specific-pattern routing keeps the exact Huma
	// patterns winning over the broad "/api/" catch-all (see newAPIFallback).
	mux.Handle("/api/", newAPIFallback(ha))

	return ha
}

// requestIDTransformer is a huma.Transformer that injects the request's correlation ID into every
// *problem.Details body before it is serialized. It is appended to cfg.Transformers in New so that all
// Huma-generated errors (422, 415, 500, …) carry a requestId that matches the Request-Id response header
// set by the request-ID middleware in the server-edge chain.
//
// Non-*problem.Details bodies (normal success responses) pass through unchanged.
func requestIDTransformer(ctx huma.Context, _ string, v any) (any, error) {
	if d, ok := v.(*problem.Details); ok && d.RequestID == "" {
		d.RequestID = httpx.RequestID(ctx.Context())
	}

	return v, nil
}

// withMaxBodyBytes returns op with MaxBodyBytes defaulted to maxBodyBytes when the caller left it unset, so
// every operation inherits the body-size ceiling without each registration callsite repeating it.
func withMaxBodyBytes(op huma.Operation) huma.Operation {
	if op.MaxBodyBytes == 0 {
		op.MaxBodyBytes = maxBodyBytes
	}

	return op
}
