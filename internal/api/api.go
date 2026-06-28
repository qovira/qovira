// Package api builds the Qovira API surface: the Huma instance mounted under /api/v1 and the registered
// operations. The mux is the caller's (the composition root's), so the SPA shares it. Errors currently render
// in Huma's default RFC 9457 ErrorModel; a later unit installs the house problem+json edge here (a
// package-level huma.NewError hook in internal/api/problem) so every generated error carries the house
// extensions.
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

	"github.com/qovira/qovira/internal/buildinfo"
)

// maxBodyBytes is the default maximum request-body size ceiling applied to every registered operation. The
// health endpoint has no body and ignores this field; future body-reading endpoints inherit the ceiling unless
// they override MaxBodyBytes explicitly in their Operation. This scaffolds the body-size guard noted in the
// composition root's TODO(security) comment. 4 MiB balances typical API payloads (JSON, small uploads) against
// server resource protection — a single endpoint can override downward or upward as its contract demands.
const maxBodyBytes int64 = 4 * 1024 * 1024

// New builds the Huma API mounted on the caller's mux under the /api/v1 prefix, registers all operations, and
// returns the huma.API for callers that need to introspect or extend it. The log parameter is reserved for
// future middleware (request-ID logging etc.) and is accepted now so the signature is stable across that
// addition without requiring a composition-root change.
func New(mux *http.ServeMux, bi buildinfo.Info, _ *slog.Logger) huma.API {
	cfg := huma.DefaultConfig("Qovira", bi.Version)

	ha := humago.NewWithPrefix(mux, "/api/v1", cfg)

	registerMeta(ha, bi)

	return ha
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
