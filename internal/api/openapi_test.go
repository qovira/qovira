package api_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestOpenAPI_SpecEndpoints verifies that the live OpenAPI spec is served at the standard Huma paths under
// /api/v1, and that the content reflects the expected OpenAPI version and format. These endpoints are
// provided for free by Huma's DefaultConfig + humago.NewWithPrefix wiring; the test asserts the surface
// is reachable (not shadowed by the fallback) and returns substantive content.
func TestOpenAPI_SpecEndpoints(t *testing.T) {
	t.Parallel()

	// newTestMux is defined in fallback_test.go — it boots api.New on a live httptest.Server
	// wrapped in the request-ID middleware, without a SPA catch-all.
	srv := newTestMux(t)
	t.Cleanup(srv.Close)

	cases := []struct {
		path            string
		wantStatus      int
		wantContentSnip string // substring that must appear in the body
	}{
		{
			path:            "/api/v1/openapi.json",
			wantStatus:      http.StatusOK,
			wantContentSnip: `"3.1`, // "openapi":"3.1.0" in JSON
		},
		{
			path:            "/api/v1/openapi.yaml",
			wantStatus:      http.StatusOK,
			wantContentSnip: "openapi: 3.1", // YAML prefix for the version field
		},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()

			resp, err := srv.Client().Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("GET %s: want %d, got %d", tc.path, tc.wantStatus, resp.StatusCode)
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}

			if !strings.Contains(string(body), tc.wantContentSnip) {
				t.Errorf("GET %s body does not contain %q; body prefix: %q",
					tc.path, tc.wantContentSnip, string(body[:min(200, len(body))]))
			}
		})
	}
}

// TestOpenAPI_DocsUI verifies that GET /api/v1/docs returns 200 HTML rendering the Stoplight Elements UI,
// without being shadowed by the fallback handler. The body assertion on <elements-api pins the renderer:
// a switch to Scalar or SwaggerUI would not emit that custom element and the test would catch it.
func TestOpenAPI_DocsUI(t *testing.T) {
	t.Parallel()

	srv := newTestMux(t)
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/api/v1/docs")
	if err != nil {
		t.Fatalf("GET /api/v1/docs: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/docs: want 200, got %d (fallback may have swallowed it)", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET /api/v1/docs: want Content-Type text/html, got %q", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	// <elements-api is the custom element Huma's Stoplight Elements renderer emits. Its presence confirms
	// the docs UI is active and no accidental renderer switch (e.g. Scalar, SwaggerUI) occurred.
	const stoplightMarker = "<elements-api"
	if !strings.Contains(string(body), stoplightMarker) {
		t.Errorf("GET /api/v1/docs body does not contain Stoplight Elements marker %q; body prefix: %q",
			stoplightMarker, string(body[:min(300, len(body))]))
	}
}
