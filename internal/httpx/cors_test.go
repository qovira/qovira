package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qovira/qovira/internal/httpx"
)

const testOrigin = "https://app.qovira-test.example"

// newTestCORSMiddleware returns a CORS middleware configured with testOrigin as the sole allowed origin.
func newTestCORSMiddleware(next http.Handler) http.Handler {
	return httpx.NewCORSMiddleware(httpx.CORSConfig{
		AllowedOrigins: []string{testOrigin},
	}, next)
}

// newSameOriginCORSMiddleware returns a CORS middleware with an empty allow-list (same-origin-default policy).
func newSameOriginCORSMiddleware(next http.Handler) http.Handler {
	return httpx.NewCORSMiddleware(httpx.CORSConfig{
		AllowedOrigins: nil,
	}, next)
}

// TestCORS_SimpleRequestAllowedOrigin verifies that a simple cross-origin GET from an allow-listed origin
// receives Access-Control-Allow-Origin set to that exact origin (never reflected arbitrarily).
func TestCORS_SimpleRequestAllowedOrigin(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newTestCORSMiddleware(inner)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/widgets", nil)
	req.Header.Set("Origin", testOrigin)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	acao := rr.Header().Get("Access-Control-Allow-Origin")
	if acao != testOrigin {
		t.Errorf("ACAO: want %q, got %q", testOrigin, acao)
	}
}

// TestCORS_SimpleRequestDeniedOrigin verifies that a simple cross-origin request from a non-allow-listed
// origin does NOT receive Access-Control-Allow-Origin (the same-origin-default denies it).
func TestCORS_SimpleRequestDeniedOrigin(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newSameOriginCORSMiddleware(inner)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/widgets", nil)
	req.Header.Set("Origin", "https://evil.example")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if acao := rr.Header().Get("Access-Control-Allow-Origin"); acao != "" {
		t.Errorf("non-allow-listed origin: ACAO must be absent, got %q", acao)
	}
}

// TestCORS_PreflightAllowedOrigin verifies that an OPTIONS preflight from an allow-listed origin returns 204
// with the correct CORS headers, short-circuiting the actual handler.
func TestCORS_PreflightAllowedOrigin(t *testing.T) {
	t.Parallel()

	// The inner handler must NOT be called on a preflight.
	innerCalled := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		innerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	h := newTestCORSMiddleware(inner)

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/widgets", nil)
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Content-Type")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Errorf("preflight status: want 204, got %d", rr.Code)
	}

	if innerCalled {
		t.Error("inner handler was called on preflight; must be short-circuited")
	}

	acao := rr.Header().Get("Access-Control-Allow-Origin")
	if acao != testOrigin {
		t.Errorf("ACAO: want %q, got %q", testOrigin, acao)
	}

	if rr.Header().Get("Access-Control-Max-Age") == "" {
		t.Error("Access-Control-Max-Age missing from preflight response")
	}
}

// TestCORS_PreflightDeniedOrigin verifies that an OPTIONS preflight from a non-allow-listed origin returns
// 204 with NO Access-Control-Allow-Origin (no reflection).
func TestCORS_PreflightDeniedOrigin(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newSameOriginCORSMiddleware(inner)

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/widgets", nil)
	req.Header.Set("Origin", "https://attacker.example")
	req.Header.Set("Access-Control-Request-Method", "DELETE")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if acao := rr.Header().Get("Access-Control-Allow-Origin"); acao != "" {
		t.Errorf("denied preflight: ACAO must be absent, got %q", acao)
	}
}

// TestCORS_NeverPairsWildcardWithCredentials verifies that the CORS middleware never emits
// Access-Control-Allow-Origin: * alongside Access-Control-Allow-Credentials: true.
func TestCORS_NeverPairsWildcardWithCredentials(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newTestCORSMiddleware(inner)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/widgets", nil)
	req.Header.Set("Origin", testOrigin)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	acao := rr.Header().Get("Access-Control-Allow-Origin")
	credentials := rr.Header().Get("Access-Control-Allow-Credentials")

	if acao == "*" && credentials == "true" {
		t.Error("CORS: * paired with Allow-Credentials: true — forbidden per spec")
	}
}

// TestCORS_ExposesRequestIDHeader verifies that the CORS middleware includes Request-Id in
// Access-Control-Expose-Headers so browser clients can read it.
func TestCORS_ExposesRequestIDHeader(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newTestCORSMiddleware(inner)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/widgets", nil)
	req.Header.Set("Origin", testOrigin)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	expose := rr.Header().Get("Access-Control-Expose-Headers")
	if expose == "" {
		t.Error("Access-Control-Expose-Headers missing; Request-Id must be exposed to browser clients")
	}

	// It must include Request-Id (case-insensitive check via simple string search is fine for a header name).
	if !containsCaseInsensitive(expose, "Request-Id") {
		t.Errorf("Access-Control-Expose-Headers does not include Request-Id: %q", expose)
	}
}

// TestCORS_NoOriginPassthrough verifies that a request without an Origin header passes through with a 200
// and no CORS headers set (same-origin requests don't set the Origin header).
func TestCORS_NoOriginPassthrough(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newTestCORSMiddleware(inner)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/widgets", nil)
	// No Origin header — same-origin browser request or server-to-server.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("no-Origin request: want 200, got %d", rr.Code)
	}

	if acao := rr.Header().Get("Access-Control-Allow-Origin"); acao != "" {
		t.Errorf("no-Origin request: ACAO must be absent, got %q", acao)
	}
}

// TestCORS_VaryOriginForCrossOriginRequests verifies that Vary: Origin is emitted for every cross-origin
// request — including one from a denied origin — so a shared cache cannot replay a response across origins.
// A request without an Origin header gets no Vary from this middleware.
func TestCORS_VaryOriginForCrossOriginRequests(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	t.Run("denied origin still varies", func(t *testing.T) {
		t.Parallel()

		h := newSameOriginCORSMiddleware(inner) // empty allow-list → every origin denied
		req := httptest.NewRequest(http.MethodGet, "/api/v1/widgets", nil)
		req.Header.Set("Origin", "https://evil.example")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		if vary := rr.Header().Get("Vary"); !containsCaseInsensitive(vary, "Origin") {
			t.Errorf("denied cross-origin request: Vary must include Origin, got %q", vary)
		}
	})

	t.Run("permitted origin varies", func(t *testing.T) {
		t.Parallel()

		h := newTestCORSMiddleware(inner)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/widgets", nil)
		req.Header.Set("Origin", testOrigin)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		if vary := rr.Header().Get("Vary"); !containsCaseInsensitive(vary, "Origin") {
			t.Errorf("permitted cross-origin request: Vary must include Origin, got %q", vary)
		}
	})

	t.Run("no Origin header gets no Vary", func(t *testing.T) {
		t.Parallel()

		h := newTestCORSMiddleware(inner)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/widgets", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		if vary := rr.Header().Get("Vary"); vary != "" {
			t.Errorf("no-Origin request: Vary must be absent, got %q", vary)
		}
	})
}

// containsCaseInsensitive checks whether s contains sub with a case-insensitive comparison.
func containsCaseInsensitive(s, sub string) bool {
	return contains(toLower(s), toLower(sub))
}

func toLower(s string) string {
	b := make([]byte, len(s))
	for i := range len(s) {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}

		b[i] = c
	}

	return string(b)
}
