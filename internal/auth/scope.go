package auth

// scope.go provides ScopeFromPrincipal — a named seam that converts an
// authenticated store.Principal into a store.Scope for data-access operations.
// Keeping the conversion explicit here (rather than inlining store.UserScope
// everywhere) makes it easy to find and update if the scoping model changes.

import "github.com/qovira/qovira/internal/store"

// ScopeFromPrincipal returns a user [store.Scope] bound to p's UserID.  It
// delegates to [store.UserScope] and exists as a named seam so that the auth
// package is the single call site for the Principal → Scope translation.
//
// The returned Scope is always a user scope (IsSystem() == false); the role is
// not carried into the Scope — role-based access control is the domain of
// [RequireAdmin] and similar middleware, not of the data-access layer.
func ScopeFromPrincipal(p store.Principal) store.Scope {
	return store.UserScope(p)
}
