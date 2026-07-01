package api

import (
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/qovira/qovira/internal/api/problem"
	"github.com/qovira/qovira/internal/httpx"
)

// newAPIFallback returns an http.Handler that catches all requests reaching the /api/ subtree that were
// NOT claimed by a Huma-registered operation or Huma's own meta routes (openapi, docs, schemas). It
// inspects the Huma operation map to distinguish:
//
//   - /api/v1/{known-path} reached with an unregistered method → 405 Method Not Allowed with Allow header
//   - /api/v1/{unknown-path} or any other /api/... path → 404 Not Found
//
// This is necessary because Huma over the humago stdlib-ServeMux adapter does NOT emit routing-level 404 or
// 405 responses — the mux owns routing and unmatched /api/* paths would otherwise fall through to the SPA
// catch-all (returning 200 HTML). Registering this handler on the "/api/" subtree pattern and relying on
// Go 1.22's most-specific-pattern routing ensures that exact Huma operation patterns win before this catch-all.
//
// Limitation: only exact-path routes are matched; templated path-param routes (e.g. /items/{id}) are not
// distinguished yet. None exist today — the callsite comment marks where to extend when they are added.
func newAPIFallback(ha huma.API) http.Handler {
	// Build the path→method set once, captured in the closure, from ha.OpenAPI().Paths. Paths is keyed by
	// the operation path WITHOUT the /api/v1 prefix (e.g. "/health"). We convert the key set to a map of
	// opPath → set of registered HTTP methods (upper-cased) at construction time so request handling is O(1).
	type methodSet = map[string]bool

	opMethods := make(map[string]methodSet)

	for opPath, item := range ha.OpenAPI().Paths {
		if item == nil {
			continue
		}

		methods := methodSet{}

		// Reflect over the PathItem to find non-nil operation fields rather than enumerating field names
		// manually — adding a new HTTP verb to Huma won't silently break this handler.
		v := reflect.ValueOf(*item)
		t := v.Type()

		for i := range v.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			fv := v.Field(i)
			// We're looking for pointer-to-Operation fields; non-nil means the method is registered.
			if fv.Kind() == reflect.Pointer && !fv.IsNil() {
				methods[strings.ToUpper(f.Name)] = true
			}
		}

		if len(methods) > 0 {
			opMethods[opPath] = methods
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := httpx.RequestID(r.Context())

		// Determine the op-relative path by stripping the /api/v1 prefix (the ServeMux prefix used when
		// registering Huma operations). Paths that don't carry the /api/v1 prefix (e.g. /api/v2/...) are
		// treated as unknown and receive a 404.
		const v1prefix = "/api/v1"

		var opPath string
		if after, ok := strings.CutPrefix(r.URL.Path, v1prefix); ok {
			opPath = after
			if opPath == "" {
				opPath = "/"
			}
		}

		if opPath != "" {
			if methods, known := opMethods[opPath]; known {
				// The path exists but the requested method is not registered → 405.
				//
				// NOTE: exact-match only. When templated routes (e.g. /items/{id}) are added, extend this
				// with a linear scan matching patterns against r.URL.Path. For now (/health only) it suffices.
				sorted := make([]string, 0, len(methods))
				for m := range methods {
					sorted = append(sorted, m)
				}
				slices.Sort(sorted)
				w.Header().Set("Allow", strings.Join(sorted, ", "))

				d := problem.MethodNotAllowed("Method " + r.Method + " is not allowed on " + r.URL.Path)
				d.RequestID = reqID
				problem.WriteJSON(w, d)
				return
			}
		}

		// Unknown path (or non-/api/v1 path) → 404.
		d := problem.NotFound("No route found for " + r.Method + " " + r.URL.Path)
		d.RequestID = reqID
		problem.WriteJSON(w, d)
	})
}
