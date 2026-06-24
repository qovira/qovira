package auth_test

// Tests for ScopeFromPrincipal and RequireAdmin.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qovira/qovira/internal/auth"
	"github.com/qovira/qovira/internal/httpx"
	"github.com/qovira/qovira/internal/store"
)

// ── ScopeFromPrincipal ────────────────────────────────────────────────────────

// TestScopeFromPrincipal_YieldsUserScope verifies that the Scope produced by ScopeFromPrincipal is accepted as a
// user scope (IsSystem=false) and carries the correct UserID.
func TestScopeFromPrincipal_YieldsUserScope(t *testing.T) {
	t.Parallel()

	p := store.Principal{UserID: "user_abc", Role: string(auth.RoleMember)}
	scope := auth.ScopeFromPrincipal(p)

	if scope.IsSystem() {
		t.Error("ScopeFromPrincipal returned a system scope; want user scope")
	}
	if scope.UserID() != p.UserID {
		t.Errorf("scope.UserID() = %q, want %q", scope.UserID(), p.UserID)
	}
}

// TestScopeFromPrincipal_AdminPrincipal_YieldsUserScope verifies that an admin Principal still yields a user
// scope (not a system scope).
func TestScopeFromPrincipal_AdminPrincipal_YieldsUserScope(t *testing.T) {
	t.Parallel()

	p := store.Principal{UserID: "admin_xyz", Role: string(auth.RoleAdmin)}
	scope := auth.ScopeFromPrincipal(p)

	if scope.IsSystem() {
		t.Error("ScopeFromPrincipal for admin returned system scope; want user scope")
	}
	if scope.UserID() != p.UserID {
		t.Errorf("scope.UserID() = %q, want %q", scope.UserID(), p.UserID)
	}
}

// ── RequireAdmin ──────────────────────────────────────────────────────────────

// TestRequireAdmin_NonAdmin_Returns403 verifies that a request with a member Principal in context is rejected
// with 403 and code "forbidden".
func TestRequireAdmin_NonAdmin_Returns403(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	h := auth.RequireAdmin(inner)

	r := httptest.NewRequest(http.MethodGet, "/admin/something", nil)
	// Inject a member principal via the httpx context helper.
	r = r.WithContext(httpx.ContextWithPrincipal(r.Context(), store.Principal{
		UserID: "user1",
		Role:   string(auth.RoleMember),
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

// TestRequireAdmin_Admin_PassesThrough verifies that an admin Principal passes through RequireAdmin unchanged.
func TestRequireAdmin_Admin_PassesThrough(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	h := auth.RequireAdmin(inner)

	r := httptest.NewRequest(http.MethodGet, "/admin/something", nil)
	r = r.WithContext(httpx.ContextWithPrincipal(r.Context(), store.Principal{
		UserID: "admin1",
		Role:   string(auth.RoleAdmin),
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

// TestRequireAdmin_NoPrincipal_Returns403 verifies that when no Principal is in context (e.g. an unauthenticated
// path that somehow reached this handler), the request is rejected with 403.
func TestRequireAdmin_NoPrincipal_Returns403(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	h := auth.RequireAdmin(inner)

	r := httptest.NewRequest(http.MethodGet, "/admin/something", nil)
	// No principal in context.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (no principal in context)", rr.Code)
	}
}
