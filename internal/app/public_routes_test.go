package app_test

// Tests for the isPublicRoute predicate changes introduced in QOV-55:
//   - POST /api/v1/auth/login is now public.
//   - GET /api/v1/auth/login is still protected.
//   - /healthz, SPA paths remain public.
//   - /api/v1/... (other paths) remain protected.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qovira/qovira/internal/app"
)

// TestIsPublicRoute_LoginEndpoint verifies that POST /api/v1/auth/login is
// exempted from auth (so an unauthenticated caller can log in) and that GET on
// the same path is still protected.
func TestIsPublicRoute_LoginEndpoint(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := testConfig(t, dir, true)
	a, err := app.New(context.Background(), cfg, discardLogger(), denyAllCtor, "test")
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	cleanupApp(t, a)

	tests := []struct {
		method     string
		path       string
		wantStatus int
		desc       string
	}{
		{
			// Public — auth does not block it; the API catch-all returns 404 since the
			// login handler does not exist yet (that's a later slice). A 404 confirms
			// that auth was bypassed: a 401 would mean auth rejected it.
			method:     http.MethodPost,
			path:       "/api/v1/auth/login",
			wantStatus: http.StatusNotFound,
			desc:       "POST /api/v1/auth/login must be public (no auth required; 404 expected, not 401)",
		},
		{
			method:     http.MethodGet,
			path:       "/api/v1/auth/login",
			wantStatus: http.StatusUnauthorized,
			desc:       "GET /api/v1/auth/login must be protected",
		},
		{
			method:     http.MethodGet,
			path:       "/healthz",
			wantStatus: http.StatusOK,
			desc:       "/healthz must remain public",
		},
		{
			method:     http.MethodGet,
			path:       "/api/v1/anything-else",
			wantStatus: http.StatusUnauthorized,
			desc:       "other /api/v1/... paths must remain protected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(tt.method, tt.path, nil)
			rr := httptest.NewRecorder()
			a.Server().Handler.ServeHTTP(rr, r)

			if rr.Code != tt.wantStatus {
				t.Errorf("%s %s: status = %d, want %d — %s", tt.method, tt.path, rr.Code, tt.wantStatus, tt.desc)
			}
		})
	}
}
