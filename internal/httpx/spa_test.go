package httpx_test

// Tests for spaHandler behaviour that apply regardless of build tag (i.e. the method gate in spa.go, which is
// untagged). Build-tag-specific tests live in spa_stub_test.go (!embed_spa) or the embed variant.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSPAFallback_405IsProblemJSON verifies that a non-GET/HEAD request to an unknown non-API path returns a
// problem+json 405 response, not the plain-text "method not allowed" body that http.Error would produce. The error
// shape must be consistent with the rest of the API's problem+json contract.
func TestSPAFallback_405IsProblemJSON(t *testing.T) {
	t.Parallel()

	h := newServerHandler(t, "dev")

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			r := httptest.NewRequest(method, "/some/spa/route", nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)

			if rr.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s: status = %d, want 405", method, rr.Code)
			}

			ct := rr.Header().Get("Content-Type")
			if ct != "application/problem+json" {
				t.Errorf("%s: Content-Type = %q, want application/problem+json", method, ct)
			}

			rawBody := rr.Body.String()
			if strings.HasPrefix(rawBody, "method not allowed") {
				t.Errorf("%s: response is plain text, want problem+json; body: %q", method, rawBody)
			}

			var p problemBody
			if err := json.NewDecoder(strings.NewReader(rawBody)).Decode(&p); err != nil {
				t.Fatalf("%s: decode problem body: %v — raw: %q", method, err, rawBody)
			}
			if p.Status != http.StatusMethodNotAllowed {
				t.Errorf("%s: problem.status = %d, want 405", method, p.Status)
			}
			if p.Code == "" {
				t.Errorf("%s: problem.code is empty", method)
			}

			if allow := rr.Header().Get("Allow"); allow == "" {
				t.Errorf("%s: Allow header missing on 405", method)
			}
		})
	}
}
