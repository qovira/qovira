//go:build !embed_spa

package httpx_test

// spa_stub_test.go — tests that run ONLY under the default (no-embed) build.
//
// These tests verify the in-code stub FS wired by spa_noembed.go:
//   - spaHandler() does not panic when constructed with the stub.
//   - GET / returns 200 with the stub index.html markup (not empty, not a panic).
//   - The stub body contains the sentinel text from the in-code stub so we can
//     confirm we are NOT serving real SvelteKit output.
//   - Cache-Control header is set to no-cache for the stub root.
//   - Non-GET methods are rejected with 405 (handler logic unchanged).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newStubServerHandler returns the http.Handler from a throwaway server for
// stub-SPA tests. This compiles only without -tags embed_spa, exercising the
// in-code stub FS path.
func newStubServerHandler(t *testing.T) http.Handler {
	t.Helper()
	return newServerHandler(t, "dev")
}

// TestSPAStub_RootReturns200 verifies that under the default (no embed_spa tag)
// build, GET / returns 200 and does not panic. This is the primary TDD guard:
// it must pass without any webdist/ directory on disk.
func TestSPAStub_RootReturns200(t *testing.T) {
	t.Parallel()

	h := newStubServerHandler(t)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

// TestSPAStub_BodyContainsStubMarkup asserts that the stub index.html served
// by the default build contains the expected sentinel phrase, confirming we are
// NOT serving a real SvelteKit build and that the in-code content is wired correctly.
func TestSPAStub_BodyContainsStubMarkup(t *testing.T) {
	t.Parallel()

	h := newStubServerHandler(t)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	body := rr.Body.String()

	// The stub must be a recognisable HTML document.
	if !strings.Contains(body, "<html") && !strings.Contains(body, "<!doctype html") {
		t.Errorf("stub body does not look like HTML: %q", body)
	}

	// The stub must mention "Qovira" so it is clearly branded, not blank.
	if !strings.Contains(body, "Qovira") {
		t.Errorf("stub body missing brand name 'Qovira': %q", body)
	}

	// The stub must indicate the web UI is not embedded, so operators know they
	// need make build for the real binary.
	lowerBody := strings.ToLower(body)
	if !strings.Contains(lowerBody, "not embedded") && !strings.Contains(lowerBody, "make build") {
		t.Errorf("stub body does not indicate the web UI is not embedded: %q", body)
	}
}

// TestSPAStub_NoCacheHeader verifies that the stub index.html carries
// Cache-Control: no-cache, consistent with the production handler behaviour.
func TestSPAStub_NoCacheHeader(t *testing.T) {
	t.Parallel()

	h := newStubServerHandler(t)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if cc := rr.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-cache")
	}
}

// TestSPAStub_ContentTypeHTML verifies the stub index.html has the correct
// Content-Type header.
func TestSPAStub_ContentTypeHTML(t *testing.T) {
	t.Parallel()

	h := newStubServerHandler(t)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	ct := rr.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", ct)
	}
}

// TestSPAStub_UnknownPathFallsBack verifies that unknown non-API paths
// still return the stub index.html (SPA fallback logic unchanged in stub mode).
func TestSPAStub_UnknownPathFallsBack(t *testing.T) {
	t.Parallel()

	h := newStubServerHandler(t)

	for _, path := range []string{"/some/route", "/dashboard", "/settings"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, path, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)

			if rr.Code != http.StatusOK {
				t.Errorf("path %s: status = %d, want 200", path, rr.Code)
			}
			body := rr.Body.String()
			if !strings.Contains(body, "Qovira") {
				t.Errorf("path %s: stub body missing 'Qovira': %q", path, body)
			}
		})
	}
}

// TestSPAStub_NonGETRejected verifies that the 405 guard in spaHandler
// still fires in stub mode — the method gate is in spa.go (untagged), not
// in the embed/noembed split.
func TestSPAStub_NonGETRejected(t *testing.T) {
	t.Parallel()

	h := newStubServerHandler(t)

	r := httptest.NewRequest(http.MethodPost, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

// TestSPAStub_ImmutableAssetCacheHeader verifies that the stub sentinel asset
// under /_app/immutable/ is served with Cache-Control: public, max-age=31536000, immutable.
// This exercises the handler's immutable-path detection on the stub FS
// (spa_noembed.go includes _app/immutable/stub.js for this purpose).
func TestSPAStub_ImmutableAssetCacheHeader(t *testing.T) {
	t.Parallel()

	h := newStubServerHandler(t)

	r := httptest.NewRequest(http.MethodGet, "/_app/immutable/stub.js", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}

	const want = "public, max-age=31536000, immutable"
	if cc := rr.Header().Get("Cache-Control"); cc != want {
		t.Errorf("Cache-Control = %q, want %q", cc, want)
	}
}
