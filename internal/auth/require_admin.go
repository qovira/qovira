package auth

// require_admin.go provides RequireAdmin, an http.Handler middleware that
// enforces the RoleAdmin check.  It reads the store.Principal from context
// (placed there by the auth middleware) and rejects non-admin callers with a
// 403 problem+json response.
//
// Import graph note: auth → httpx is intentional and cycle-free.
// httpx does NOT import auth; auth imports httpx only for the WriteProblem
// helper and the PrincipalFromContext accessor.

import (
	"net/http"

	"github.com/qovira/qovira/internal/httpx"
)

// RequireAdmin is an http.Handler middleware that allows only requests whose
// context carries a [store.Principal] with Role == [RoleAdmin].  All other
// requests — including those with no Principal at all — receive a 403
// problem+json response with code "forbidden" before the wrapped handler runs.
//
// Usage: mount it as an inner middleware after auth (which populates the
// Principal):
//
//	r.Handle("GET /admin/...", auth.RequireAdmin(myAdminHandler))
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := httpx.PrincipalFromContext(r.Context())
		if !ok || p.Role != string(RoleAdmin) {
			httpx.WriteProblem(w, r, httpx.Problem{
				Title:  "Forbidden",
				Status: http.StatusForbidden,
				Detail: "This action requires administrator privileges.",
				Code:   "forbidden",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}
