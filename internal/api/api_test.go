package api_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qovira/qovira/internal/api"
	"github.com/qovira/qovira/internal/buildinfo"
)

// TestHealth_ResponseShape verifies that GET /api/v1/health returns 200 with an application/json body
// carrying the required fields: status, version, commit, and buildTime.
func TestHealth_ResponseShape(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	bi := buildinfo.Info{
		Version:   "v0.1.0",
		Commit:    "abc1234",
		BuildTime: "2024-01-15T10:00:00Z",
		GoVersion: "go1.26.4",
	}

	api.New(mux, bi, slog.Default())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/health: want 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	ct := rr.Header().Get("Content-Type")
	if ct == "" {
		t.Error("Content-Type header is missing")
	}

	var payload struct {
		Status    string `json:"status"`
		Version   string `json:"version"`
		Commit    string `json:"commit"`
		BuildTime string `json:"buildTime"`
	}

	if err := json.NewDecoder(rr.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response body: %v", err)
	}

	if payload.Status != "ok" {
		t.Errorf("status: want %q, got %q", "ok", payload.Status)
	}

	if payload.Version != "v0.1.0" {
		t.Errorf("version: want %q, got %q", "v0.1.0", payload.Version)
	}

	if payload.Commit != "abc1234" {
		t.Errorf("commit: want %q, got %q", "abc1234", payload.Commit)
	}

	if payload.BuildTime != "2024-01-15T10:00:00Z" {
		t.Errorf("buildTime: want %q, got %q", "2024-01-15T10:00:00Z", payload.BuildTime)
	}
}

// TestHealth_NoHealthzRegistration verifies that api.New does not register a /healthz handler by confirming
// the mux only routes /api/v1/... patterns through Huma. Without a SPA catch-all, /healthz must 404.
func TestHealth_NoHealthzRegistration(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	api.New(mux, buildinfo.Info{Version: "(devel)", Commit: "x", GoVersion: "go1.26"}, slog.Default())

	// No SPA catch-all — /healthz must 404.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("GET /healthz (no SPA): want 404, got %d", rr.Code)
	}
}

// TestNew_ReturnsMountedAPI verifies that api.New returns a non-nil huma.API value.
func TestNew_ReturnsMountedAPI(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	humaAPI := api.New(mux, buildinfo.Info{Version: "(devel)", Commit: "x", GoVersion: "go1.26"}, slog.Default())

	if humaAPI == nil {
		t.Error("api.New returned nil")
	}
}
