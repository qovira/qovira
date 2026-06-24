package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qovira/qovira/internal/httpx"
)

// TestSessionTokenFromRequest_CookieFirst verifies that the session cookie takes precedence over the Authorization:
// Bearer header when both are present, and that viaCookie is true.
func TestSessionTokenFromRequest_CookieFirst(t *testing.T) {
	t.Parallel()

	const cookieToken = "qov_cookiewins0000000000000000000000000000" //nolint:gosec // test fixture
	const bearerToken = "qov_bearerlosesssssssssssssssssssssssssss"  //nolint:gosec // test fixture

	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.AddCookie(&http.Cookie{Name: httpx.SessionCookieName, Value: cookieToken}) //nolint:gosec // G124: test request, Secure/SameSite not relevant for httptest
	r.Header.Set("Authorization", "Bearer "+bearerToken)

	got, viaCookie := httpx.SessionTokenFromRequest(r)

	if got != cookieToken {
		t.Errorf("token = %q, want cookie token %q", got, cookieToken)
	}
	if !viaCookie {
		t.Error("viaCookie = false, want true (cookie should win)")
	}
}

// TestSessionTokenFromRequest_BearerFallback verifies that when no session cookie is present, the Bearer token is
// returned and viaCookie is false.
func TestSessionTokenFromRequest_BearerFallback(t *testing.T) {
	t.Parallel()

	const token = "qov_bearertoken000000000000000000000000000" //nolint:gosec // test fixture

	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.Header.Set("Authorization", "Bearer "+token)

	got, viaCookie := httpx.SessionTokenFromRequest(r)

	if got != token {
		t.Errorf("token = %q, want %q", got, token)
	}
	if viaCookie {
		t.Error("viaCookie = true, want false (Bearer fallback)")
	}
}

// TestSessionTokenFromRequest_NeitherPresent verifies that when neither cookie nor Bearer header is present, an empty
// token is returned.
func TestSessionTokenFromRequest_NeitherPresent(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)

	got, viaCookie := httpx.SessionTokenFromRequest(r)

	if got != "" {
		t.Errorf("token = %q, want empty string", got)
	}
	if viaCookie {
		t.Error("viaCookie = true, want false when no token present")
	}
}

// TestSessionTokenFromRequest_EmptyCookieFallsBackToBearer verifies that an empty session cookie value falls back to
// the Bearer header.
func TestSessionTokenFromRequest_EmptyCookieFallsBackToBearer(t *testing.T) {
	t.Parallel()

	const token = "qov_bearertoken000000000000000000000000000" //nolint:gosec // test fixture

	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.AddCookie(&http.Cookie{Name: httpx.SessionCookieName, Value: ""}) //nolint:gosec // G124: test request only
	r.Header.Set("Authorization", "Bearer "+token)

	got, viaCookie := httpx.SessionTokenFromRequest(r)

	if got != token {
		t.Errorf("token = %q, want Bearer token %q", got, token)
	}
	if viaCookie {
		t.Error("viaCookie = true, want false (empty cookie should fall back to Bearer)")
	}
}
