package httpx_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qovira/qovira/internal/events"
	"github.com/qovira/qovira/internal/httpx"
)

// healthzBody is the JSON shape returned by GET /healthz.
type healthzBody struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// newServerHandler returns the http.Handler from a throwaway NewServer call.
// It does NOT start a listener — it only builds the handler for unit tests.
// A fresh events.NewBus() is injected so the server compiles and routes
// correctly; SSE-specific behaviour is tested in events_test.go.
// No middleware is passed so route-level behaviour is tested in isolation.
func newServerHandler(t *testing.T, version string) http.Handler {
	t.Helper()
	router := httpx.NewRouter()
	srv := httpx.NewServer("127.0.0.1:0", version, router, events.NewBus())
	return srv.Handler
}

// TestNewServer_Addr verifies that the Addr field on the returned *http.Server
// equals the addr argument passed to NewServer.
func TestNewServer_Addr(t *testing.T) {
	t.Parallel()

	const addr = "127.0.0.1:8080"
	router := httpx.NewRouter()
	srv := httpx.NewServer(addr, "v1.2.3", router, events.NewBus())
	if srv.Addr != addr {
		t.Errorf("Addr = %q, want %q", srv.Addr, addr)
	}
}

// TestNewServer_ReadHeaderTimeout verifies that NewServer sets ReadHeaderTimeout
// to a non-zero value (required to avoid the gosec G112 slow-loris finding).
func TestNewServer_ReadHeaderTimeout(t *testing.T) {
	t.Parallel()

	router := httpx.NewRouter()
	srv := httpx.NewServer("127.0.0.1:0", "dev", router, events.NewBus())
	if srv.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout is zero; gosec G112 requires a non-zero value")
	}
}

// TestHealthz_OK verifies Acceptance Criterion 2: GET /healthz returns 200,
// Content-Type: application/json, and a body with status:"ok" and the
// configured version.
func TestHealthz_OK(t *testing.T) {
	t.Parallel()

	const version = "v0.1.0-test"
	h := newServerHandler(t, version)

	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}

	ct := rr.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body healthzBody
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode healthz body: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("body.status = %q, want %q", body.Status, "ok")
	}
	if body.Version != version {
		t.Errorf("body.version = %q, want %q", body.Version, version)
	}
}

// TestAPIUnknownPath_JSONProblem verifies Acceptance Criterion 3 (API side):
// an unknown /api/v1/... path returns a JSON 404 problem+json response, never
// index.html.
func TestAPIUnknownPath_JSONProblem(t *testing.T) {
	t.Parallel()

	h := newServerHandler(t, "dev")

	paths := []string{
		"/api/v1/does-not-exist",
		"/api/v1/foo/bar",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, path, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)

			if rr.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rr.Code)
			}

			ct := rr.Header().Get("Content-Type")
			if ct != "application/problem+json" {
				t.Errorf("Content-Type = %q, want application/problem+json", ct)
			}

			// Body must not be index.html — check for HTML doctype.
			rawBody := rr.Body.String()
			if strings.Contains(rawBody, "<!doctype html") || strings.Contains(rawBody, "<html") {
				t.Errorf("API 404 returned HTML body: %s", rawBody)
			}

			var p problemBody
			if err := json.NewDecoder(strings.NewReader(rawBody)).Decode(&p); err != nil {
				t.Fatalf("decode problem body: %v", err)
			}
			if p.Status != http.StatusNotFound {
				t.Errorf("problem.status = %d, want 404", p.Status)
			}
			if p.Code == "" {
				t.Error("problem.code is empty")
			}
		})
	}
}

// TestSPAFallback_ServeIndexHTML verifies Acceptance Criterion 3 (SPA side):
// unknown non-API paths return index.html so the SvelteKit client-side router
// can handle them.
func TestSPAFallback_ServeIndexHTML(t *testing.T) {
	t.Parallel()

	h := newServerHandler(t, "dev")

	paths := []string{
		"/",
		"/dashboard",
		"/settings/profile",
		"/unknown-path",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, path, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)

			if rr.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rr.Code)
			}

			body := rr.Body.String()
			if !strings.Contains(body, "<html") && !strings.Contains(body, "<!doctype html") {
				t.Errorf("response body does not look like index.html: %q", body)
			}
		})
	}
}

// TestSPAFallback_NoCacheHeader verifies Acceptance Criterion 4 (SPA):
// index.html is served with Cache-Control: no-cache.
func TestSPAFallback_NoCacheHeader(t *testing.T) {
	t.Parallel()

	h := newServerHandler(t, "dev")

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	cc := rr.Header().Get("Cache-Control")
	if cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-cache")
	}
}

// NOTE: the immutable long-lived cache header on real files under
// /_app/immutable/ is asserted in spa_stub_test.go (tagged !embed_spa), since
// the concrete asset filename varies by build (stub asset vs. real SvelteKit
// chunk). The build-agnostic fallback (directory request → index.html, no
// immutable header) is covered by the tests above.

// TestNoCORSHeaders verifies Acceptance Criterion 5: no CORS headers are
// emitted by default on any response, since the SPA is same-origin.
func TestNoCORSHeaders(t *testing.T) {
	t.Parallel()

	h := newServerHandler(t, "dev")

	corsHeaders := []string{
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Methods",
		"Access-Control-Allow-Headers",
		"Access-Control-Allow-Credentials",
		"Access-Control-Expose-Headers",
		"Access-Control-Max-Age",
	}

	paths := []string{
		"/healthz",
		"/api/v1/does-not-exist",
		"/",
		"/events",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, path, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)

			for _, h := range corsHeaders {
				if v := rr.Header().Get(h); v != "" {
					t.Errorf("path %s: unexpected CORS header %s: %q", path, h, v)
				}
			}
		})
	}
}

// TestEventsRoute_IsMatchedNotSPA verifies that /events is reserved for every
// method and NOT swallowed by the SPA fallback — neither GET nor a non-idempotent
// verb may return index.html.
func TestEventsRoute_IsMatchedNotSPA(t *testing.T) {
	t.Parallel()

	h := newServerHandler(t, "dev")

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(method, "/events", nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)

			// Neither the stub nor real SPA index.html may be returned for /events.
			body := rr.Body.String()
			if strings.Contains(body, "<!doctype html") || strings.Contains(body, "<html") {
				t.Errorf("%s /events returned index.html — route not reserved or SPA fallback swallowed it", method)
			}
		})
	}
}

// TestImmutableDir_NoListing verifies that requesting the asset directory
// itself does not produce a directory listing: the embedded asset tree must
// never be enumerable, and a directory request must not be stamped with the
// immutable cache header. It falls back to index.html (no-cache) instead.
func TestImmutableDir_NoListing(t *testing.T) {
	t.Parallel()

	h := newServerHandler(t, "dev")

	for _, path := range []string{"/_app/immutable/", "/_app/", "/_app"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, path, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)

			body := rr.Body.String()
			// A directory listing from http.FileServer is an HTML <pre> with
			// <a href> entries — check that no directory-listing markup appeared
			// (the handler falls back to index.html for directory paths).
			if strings.Contains(body, "<pre>") && strings.Contains(body, "<a href") {
				t.Errorf("path %s leaked a directory listing: %q", path, body)
			}
			// The long-lived immutable header must not be attached to a
			// directory request.
			if cc := rr.Header().Get("Cache-Control"); cc == "public, max-age=31536000, immutable" {
				t.Errorf("path %s got the immutable cache header on a directory request", path)
			}
		})
	}
}

// TestSPAFallback_RejectsNonGET verifies that a non-idempotent method to an
// unknown non-API path is rejected with 405 rather than served the SPA HTML.
func TestSPAFallback_RejectsNonGET(t *testing.T) {
	t.Parallel()

	h := newServerHandler(t, "dev")

	r := httptest.NewRequest(http.MethodPost, "/some/spa/route", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, "<!doctype html") || strings.Contains(body, "<html") {
		t.Error("POST to SPA path returned index.html; want 405")
	}
}

// TestMiddlewareChain_ComposesInOrder verifies the Chain helper: middleware
// wraps the handler left-to-right, so the first middleware in the slice is the
// outermost wrapper (runs first on the way in).
func TestMiddlewareChain_ComposesInOrder(t *testing.T) {
	t.Parallel()

	var order []string
	makeMW := func(label string) httpx.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, label+":before")
				next.ServeHTTP(w, r)
				order = append(order, label+":after")
			})
		}
	}

	inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		order = append(order, "handler")
	})

	chained := httpx.Chain(inner, makeMW("A"), makeMW("B"))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rw := httptest.NewRecorder()
	chained.ServeHTTP(rw, r)

	want := []string{"A:before", "B:before", "handler", "B:after", "A:after"}
	if len(order) != len(want) {
		t.Fatalf("execution order = %v, want %v", order, want)
	}
	for i, v := range want {
		if order[i] != v {
			t.Errorf("order[%d] = %q, want %q", i, order[i], v)
		}
	}
}

// TestIntegration_ServerBindsAndAnswersHealthz is an optional integration test
// that binds a real listener on 127.0.0.1:0 (OS-assigned port) and verifies
// the server responds to GET /healthz.
func TestIntegration_ServerBindsAndAnswersHealthz(t *testing.T) {
	t.Parallel()

	const version = "v0.0.0-integration"
	router := httpx.NewRouter()
	srv := httpx.NewServer("127.0.0.1:0", version, router, events.NewBus())

	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var body healthzBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Version != version {
		t.Errorf("version = %q, want %q", body.Version, version)
	}
}
