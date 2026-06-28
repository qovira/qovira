// Package api builds the Qovira API surface: the Huma instance mounted under /api/v1 and the registered
// operations. The mux is the caller's (the composition root's), so the SPA shares it.
//
// Every error the API produces — 422 validation, 415 unsupported media type, 500 panics, and routing-level
// 404/405 — emerges as application/problem+json in the house RFC 9457 shape (type, title, status, detail,
// code, requestId, and for validation an errors[] of RFC 6901 JSON Pointers with per-field codes). No
// per-handler work is required:
//
//   - Huma errors (422, 415, 500) are shaped by the package-level huma.NewError override installed in
//     internal/api/problem's init() function, and the requestId is injected by requestIDTransformer.
//   - Routing-level 404/405 on /api/* paths that Huma's ServeMux adapter does not own are handled by
//     newAPIFallback, registered after Huma operations so Go's most-specific-pattern routing keeps the
//     exact Huma patterns winning.
//
// Go 1.22 most-specific-pattern routing on the shared ServeMux sends /api/v1/... requests (and Huma's own
// /api/v1/openapi.json, /api/v1/docs) to Huma, while everything else falls through to the SPA catch-all
// registered separately by the composition root. No manual prefix-stripping is needed.
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

// maxBodyBytes is the default maximum request-body size ceiling applied to every registered operation. The
// health endpoint has no body and ignores this field; future body-reading endpoints inherit the ceiling unless
// they override MaxBodyBytes explicitly in their Operation. This scaffolds the body-size guard noted in the
// composition root's TODO(security) comment. 4 MiB balances typical API payloads (JSON, small uploads) against
// server resource protection — a single endpoint can override downward or upward as its contract demands.
const maxBodyBytes int64 = 4 * 1024 * 1024

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

	ha := humago.NewWithPrefix(mux, "/api/v1", cfg)

	registerMeta(ha, bi)

	// Register the /api/ subtree fallback AFTER Huma operations so Go 1.22's most-specific-pattern routing
	// keeps the exact Huma patterns (e.g. "GET /api/v1/health") winning over the broader "/api/" pattern.
	// The fallback handles routing-level 404 (unknown path) and 405 (known path, wrong method) for any
	// request that reaches /api/... but was not claimed by a Huma-registered operation.
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

// withMaxBodyBytes returns op with MaxBodyBytes set to maxBodyBytes when the caller did not specify one. This
// is the house helper every file in this package should use instead of constructing huma.Operation directly —
// it ensures the body-size ceiling is always applied without each registration callsite having to remember it.
func withMaxBodyBytes(op huma.Operation) huma.Operation {
	if op.MaxBodyBytes == 0 {
		op.MaxBodyBytes = maxBodyBytes
	}

	return op
}
