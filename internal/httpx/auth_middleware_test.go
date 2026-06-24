package httpx_test

// Tests for the cookie-first extraction and CSRF double-submit check added to
// AuthMiddleware. These tests complement the existing
// middleware_test.go which covers the Bearer-only path.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qovira/qovira/internal/httpx"
	"github.com/qovira/qovira/internal/store"
)

// ── Cookie-first extraction ───────────────────────────────────────────────────

// TestAuthMiddleware_CookieToken_SetsPrincipal verifies that a session cookie
// alone (no Authorization header) authenticates the request.
func TestAuthMiddleware_CookieToken_SetsPrincipal(t *testing.T) {
	t.Parallel()

	const token = "qov_cookietoken000000000000000000000000000"
	wantPrincipal := store.Principal{UserID: "u_cookie", Role: "member"}
	validator := &fakeValidator{expectedToken: token, principal: wantPrincipal}
	isPublic := func(*http.Request) bool { return false }

	var gotPrincipal store.Principal
	var gotOK bool
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotPrincipal, gotOK = httpx.PrincipalFromContext(r.Context())
	})

	h := httpx.Chain(inner, httpx.AuthMiddleware(validator, isPublic))

	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.AddCookie(&http.Cookie{Name: httpx.SessionCookieName, Value: token, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code == http.StatusUnauthorized {
		t.Fatalf("got 401, want authenticated response")
	}
	if !gotOK {
		t.Error("PrincipalFromContext returned ok=false for cookie-authenticated request")
	}
	if gotPrincipal != wantPrincipal {
		t.Errorf("principal = %+v, want %+v", gotPrincipal, wantPrincipal)
	}
}

// TestAuthMiddleware_CookieWinsOverBearer verifies that when both a session
// cookie and an Authorization: Bearer header are present, the cookie wins.
func TestAuthMiddleware_CookieWinsOverBearer(t *testing.T) {
	t.Parallel()

	const cookieToken = "qov_cookiewins0000000000000000000000000000" //nolint:gosec // G101 false positive: test fixture, not a real credential
	const bearerToken = "qov_bearerlosesssssssssssssssssssssssssss"  //nolint:gosec // G101 false positive: test fixture, not a real credential
	wantPrincipal := store.Principal{UserID: "u_from_cookie", Role: "member"}

	// Only the cookie token is valid; bearer token will fail validation.
	validator := &fakeValidator{expectedToken: cookieToken, principal: wantPrincipal}
	isPublic := func(*http.Request) bool { return false }

	var gotPrincipal store.Principal
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotPrincipal, _ = httpx.PrincipalFromContext(r.Context())
	})

	h := httpx.Chain(inner, httpx.AuthMiddleware(validator, isPublic))

	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.AddCookie(&http.Cookie{Name: httpx.SessionCookieName, Value: cookieToken, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	r.Header.Set("Authorization", "Bearer "+bearerToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code == http.StatusUnauthorized {
		t.Fatalf("got 401, want authenticated response (cookie should win)")
	}
	if gotPrincipal != wantPrincipal {
		t.Errorf("principal = %+v, want principal from cookie %+v", gotPrincipal, wantPrincipal)
	}
}

// TestAuthMiddleware_BearerFallback verifies that when there is no session
// cookie, the Bearer token is used as a fallback.
func TestAuthMiddleware_BearerFallback(t *testing.T) {
	t.Parallel()

	const token = "qov_bearertoken000000000000000000000000000" //nolint:gosec // G101 false positive: test fixture, not a real credential
	wantPrincipal := store.Principal{UserID: "u_bearer", Role: "member"}
	validator := &fakeValidator{expectedToken: token, principal: wantPrincipal}
	isPublic := func(*http.Request) bool { return false }

	var gotOK bool
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, gotOK = httpx.PrincipalFromContext(r.Context())
	})

	h := httpx.Chain(inner, httpx.AuthMiddleware(validator, isPublic))

	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	// No cookie.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code == http.StatusUnauthorized {
		t.Fatalf("got 401, want authenticated response via Bearer fallback")
	}
	if !gotOK {
		t.Error("PrincipalFromContext returned ok=false for Bearer-authenticated request")
	}
}

// TestAuthMiddleware_NeitherCookieNorBearer_Returns401 verifies that when
// neither a session cookie nor a Bearer token is present, the response is 401.
func TestAuthMiddleware_NeitherCookieNorBearer_Returns401(t *testing.T) {
	t.Parallel()

	validator := &fakeValidator{expectedToken: "irrelevant", principal: store.Principal{}}
	isPublic := func(*http.Request) bool { return false }

	h := httpx.Chain(noopHandler(200, "ok"), httpx.AuthMiddleware(validator, isPublic))

	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

// ── CSRF double-submit ────────────────────────────────────────────────────────

// TestAuthMiddleware_CSRF_CookieUnsafeMethod_MissingHeader_Returns403 verifies
// that a POST authenticated via cookie with no CSRF-Token header returns 403.
func TestAuthMiddleware_CSRF_CookieUnsafeMethod_MissingHeader_Returns403(t *testing.T) {
	t.Parallel()

	const token = "qov_csrftest000000000000000000000000000000" //nolint:gosec // G101 false positive: test fixture, not a real credential
	const csrfValue = "csrfvalue123"
	validator := &fakeValidator{
		expectedToken: token,
		principal:     store.Principal{UserID: "u1", Role: "member"},
	}
	isPublic := func(*http.Request) bool { return false }

	h := httpx.Chain(noopHandler(200, "ok"), httpx.AuthMiddleware(validator, isPublic))

	r := httptest.NewRequest(http.MethodPost, "/api/v1/items", nil)
	r.AddCookie(&http.Cookie{Name: httpx.SessionCookieName, Value: token, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	r.AddCookie(&http.Cookie{Name: httpx.CSRFCookieName, Value: csrfValue, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	// No CSRF-Token header → 403.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (missing CSRF header)", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

// TestAuthMiddleware_CSRF_CookieUnsafeMethod_MismatchedHeader_Returns403 verifies
// that a POST authenticated via cookie with a wrong CSRF-Token header returns 403.
func TestAuthMiddleware_CSRF_CookieUnsafeMethod_MismatchedHeader_Returns403(t *testing.T) {
	t.Parallel()

	const token = "qov_csrfmismatch0000000000000000000000000" //nolint:gosec // G101 false positive: test fixture, not a real credential
	const csrfValue = "correct-csrf-value"
	validator := &fakeValidator{
		expectedToken: token,
		principal:     store.Principal{UserID: "u2", Role: "member"},
	}
	isPublic := func(*http.Request) bool { return false }

	h := httpx.Chain(noopHandler(200, "ok"), httpx.AuthMiddleware(validator, isPublic))

	r := httptest.NewRequest(http.MethodPost, "/api/v1/items", nil)
	r.AddCookie(&http.Cookie{Name: httpx.SessionCookieName, Value: token, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	r.AddCookie(&http.Cookie{Name: httpx.CSRFCookieName, Value: csrfValue, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	r.Header.Set(httpx.CSRFHeaderName, "wrong-csrf-value")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (mismatched CSRF)", rr.Code)
	}
}

// TestAuthMiddleware_CSRF_CookieUnsafeMethod_MatchingHeader_Passes verifies
// that a POST authenticated via cookie with a matching CSRF-Token header passes.
func TestAuthMiddleware_CSRF_CookieUnsafeMethod_MatchingHeader_Passes(t *testing.T) {
	t.Parallel()

	const token = "qov_csrfmatch0000000000000000000000000000" //nolint:gosec // G101 false positive: test fixture, not a real credential
	const csrfValue = "exactly-matching-csrf-value"
	validator := &fakeValidator{
		expectedToken: token,
		principal:     store.Principal{UserID: "u3", Role: "member"},
	}
	isPublic := func(*http.Request) bool { return false }

	var routeRan bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		routeRan = true
		w.WriteHeader(http.StatusOK)
	})

	h := httpx.Chain(inner, httpx.AuthMiddleware(validator, isPublic))

	r := httptest.NewRequest(http.MethodPost, "/api/v1/items", nil)
	r.AddCookie(&http.Cookie{Name: httpx.SessionCookieName, Value: token, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	r.AddCookie(&http.Cookie{Name: httpx.CSRFCookieName, Value: csrfValue, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	r.Header.Set(httpx.CSRFHeaderName, csrfValue)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (matching CSRF)", rr.Code)
	}
	if !routeRan {
		t.Error("route handler did not run for matching CSRF")
	}
}

// TestAuthMiddleware_CSRF_BearerUnsafeMethod_NoCSRFRequired verifies that a
// POST authenticated via Bearer (not cookie) does NOT require a CSRF header.
func TestAuthMiddleware_CSRF_BearerUnsafeMethod_NoCSRFRequired(t *testing.T) {
	t.Parallel()

	const token = "qov_bearernocs0000000000000000000000000000" //nolint:gosec // G101 false positive: test fixture, not a real credential
	validator := &fakeValidator{
		expectedToken: token,
		principal:     store.Principal{UserID: "u4", Role: "member"},
	}
	isPublic := func(*http.Request) bool { return false }

	var routeRan bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		routeRan = true
		w.WriteHeader(http.StatusOK)
	})

	h := httpx.Chain(inner, httpx.AuthMiddleware(validator, isPublic))

	r := httptest.NewRequest(http.MethodPost, "/api/v1/items", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	// No CSRF header, no CSRF cookie — should be fine for Bearer auth.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (Bearer-authed POST, no CSRF needed)", rr.Code)
	}
	if !routeRan {
		t.Error("route handler did not run for Bearer-authed POST")
	}
}

// TestAuthMiddleware_CSRF_CookieGetMethod_Exempt verifies that a GET request
// authenticated via cookie is NOT subject to CSRF (safe method exemption).
func TestAuthMiddleware_CSRF_CookieGetMethod_Exempt(t *testing.T) {
	t.Parallel()

	const token = "qov_getexempt000000000000000000000000000" //nolint:gosec // G101 false positive: test fixture, not a real credential
	validator := &fakeValidator{
		expectedToken: token,
		principal:     store.Principal{UserID: "u5", Role: "member"},
	}
	isPublic := func(*http.Request) bool { return false }

	var routeRan bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		routeRan = true
		w.WriteHeader(http.StatusOK)
	})

	h := httpx.Chain(inner, httpx.AuthMiddleware(validator, isPublic))

	r := httptest.NewRequest(http.MethodGet, "/api/v1/items", nil)
	r.AddCookie(&http.Cookie{Name: httpx.SessionCookieName, Value: token, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode}) //nolint:gosec // G124 false positive: test-only fake request, not a real browser cookie
	// No CSRF header or cookie — GET is exempt.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (GET is CSRF-exempt)", rr.Code)
	}
	if !routeRan {
		t.Error("route handler did not run for GET (CSRF-exempt)")
	}
}

// TestAuthMiddleware_CSRF_UnsafeMethods verifies that POST, PATCH, and DELETE
// all require CSRF when authenticated via cookie.
func TestAuthMiddleware_CSRF_UnsafeMethods(t *testing.T) {
	t.Parallel()

	const token = "qov_unsafemethods000000000000000000000000" //nolint:gosec // G101 false positive: test fixture, not a real credential
	const csrfValue = "csrf-for-unsafe"
	validator := &fakeValidator{
		expectedToken: token,
		principal:     store.Principal{UserID: "u6", Role: "member"},
	}
	isPublic := func(*http.Request) bool { return false }

	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			h := httpx.Chain(noopHandler(200, "ok"), httpx.AuthMiddleware(validator, isPublic))

			// First: no CSRF header → 403.
			r := httptest.NewRequest(method, "/api/v1/items", nil)
			r.AddCookie(&http.Cookie{Name: httpx.SessionCookieName, Value: token, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})  //nolint:gosec // G124: test-only fake request
			r.AddCookie(&http.Cookie{Name: httpx.CSRFCookieName, Value: csrfValue, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode}) //nolint:gosec // G124: test-only fake request
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)
			if rr.Code != http.StatusForbidden {
				t.Errorf("%s (no CSRF header): status = %d, want 403", method, rr.Code)
			}

			// Second: matching CSRF header → 200.
			r2 := httptest.NewRequest(method, "/api/v1/items", nil)
			r2.AddCookie(&http.Cookie{Name: httpx.SessionCookieName, Value: token, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			r2.AddCookie(&http.Cookie{Name: httpx.CSRFCookieName, Value: csrfValue, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			r2.Header.Set(httpx.CSRFHeaderName, csrfValue)
			rr2 := httptest.NewRecorder()
			h.ServeHTTP(rr2, r2)
			if rr2.Code != http.StatusOK {
				t.Errorf("%s (matching CSRF header): status = %d, want 200", method, rr2.Code)
			}
		})
	}
}

// TestSessionCookieName_IsExported verifies the exported constant value.
func TestSessionCookieName_IsExported(t *testing.T) {
	t.Parallel()

	if httpx.SessionCookieName != "__Host-qovira_session" {
		t.Errorf("SessionCookieName = %q, want __Host-qovira_session", httpx.SessionCookieName)
	}
}

// TestCSRFCookieName_IsExported verifies the exported constant value.
func TestCSRFCookieName_IsExported(t *testing.T) {
	t.Parallel()

	if httpx.CSRFCookieName != "qovira_csrf" {
		t.Errorf("CSRFCookieName = %q, want qovira_csrf", httpx.CSRFCookieName)
	}
}

// TestCSRFHeaderName_IsExported verifies the exported constant value.
func TestCSRFHeaderName_IsExported(t *testing.T) {
	t.Parallel()

	if httpx.CSRFHeaderName != "CSRF-Token" {
		t.Errorf("CSRFHeaderName = %q, want CSRF-Token", httpx.CSRFHeaderName)
	}
}

// TestAuthMiddleware_CSRF_PUT_RequiresCSRF verifies that PUT — an unsafe,
// state-changing method — requires CSRF validation when the request is
// authenticated via a session cookie. Previously PUT was absent from
// isUnsafeMethod so it slipped through without a CSRF check.
func TestAuthMiddleware_CSRF_PUT_RequiresCSRF(t *testing.T) {
	t.Parallel()

	const token = "qov_putcsrf000000000000000000000000000000" //nolint:gosec // G101 false positive: test fixture
	const csrfValue = "csrf-put-value"
	validator := &fakeValidator{
		expectedToken: token,
		principal:     store.Principal{UserID: "u_put", Role: "member"},
	}
	isPublic := func(*http.Request) bool { return false }

	h := httpx.Chain(noopHandler(200, "ok"), httpx.AuthMiddleware(validator, isPublic))

	// PUT without CSRF header → must be 403 (not 200).
	r := httptest.NewRequest(http.MethodPut, "/api/v1/resource/1", nil)
	r.AddCookie(&http.Cookie{Name: httpx.SessionCookieName, Value: token, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})  //nolint:gosec // G124: test-only
	r.AddCookie(&http.Cookie{Name: httpx.CSRFCookieName, Value: csrfValue, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode}) //nolint:gosec // G124: test-only
	// No CSRF-Token header.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusForbidden {
		t.Errorf("PUT without CSRF header: status = %d, want 403", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}

	// PUT with matching CSRF header → must pass (200).
	r2 := httptest.NewRequest(http.MethodPut, "/api/v1/resource/1", nil)
	r2.AddCookie(&http.Cookie{Name: httpx.SessionCookieName, Value: token, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	r2.AddCookie(&http.Cookie{Name: httpx.CSRFCookieName, Value: csrfValue, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	r2.Header.Set(httpx.CSRFHeaderName, csrfValue)
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, r2)

	if rr2.Code != http.StatusOK {
		t.Errorf("PUT with matching CSRF header: status = %d, want 200", rr2.Code)
	}
}

// TestAuthMiddleware_CSRF_SafeMethods_ExemptFromCSRF verifies that GET, HEAD,
// and OPTIONS remain exempt from CSRF even when authenticated via cookie.
func TestAuthMiddleware_CSRF_SafeMethods_ExemptFromCSRF(t *testing.T) {
	t.Parallel()

	const token = "qov_safemethods000000000000000000000000000" //nolint:gosec // G101 false positive: test fixture
	validator := &fakeValidator{
		expectedToken: token,
		principal:     store.Principal{UserID: "u_safe", Role: "member"},
	}
	isPublic := func(*http.Request) bool { return false }

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			h := httpx.Chain(noopHandler(200, "ok"), httpx.AuthMiddleware(validator, isPublic))

			r := httptest.NewRequest(method, "/api/v1/resource", nil)
			r.AddCookie(&http.Cookie{Name: httpx.SessionCookieName, Value: token, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode}) //nolint:gosec // G124: test-only
			// No CSRF cookie or header — safe methods must be exempt.
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)

			if rr.Code == http.StatusForbidden {
				t.Errorf("%s: got 403, safe methods must be CSRF-exempt", method)
			}
		})
	}
}
