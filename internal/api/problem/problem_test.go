package problem_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/qovira/qovira/internal/api/problem"
)

// ---------------------------------------------------------------------------
// Registry: From maps status → correct code / title / type / status fields.
// ---------------------------------------------------------------------------

func TestFrom_RegistryMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status    int
		wantCode  string
		wantTitle string
		wantType  string
	}{
		{http.StatusUnprocessableEntity, "validation_error", "Validation Error", "https://qovira.ai/errors/validation-error"},
		{http.StatusNotFound, "not_found", "Not Found", "https://qovira.ai/errors/not-found"},
		{http.StatusMethodNotAllowed, "method_not_allowed", "Method Not Allowed", "https://qovira.ai/errors/method-not-allowed"},
		{http.StatusUnsupportedMediaType, "unsupported_media_type", "Unsupported Media Type", "https://qovira.ai/errors/unsupported-media-type"},
		{http.StatusInternalServerError, "internal_error", "Internal Error", "https://qovira.ai/errors/internal-error"},
	}

	for _, tc := range tests {
		t.Run(tc.wantCode, func(t *testing.T) {
			t.Parallel()
			d := problem.From(tc.status, "test detail")
			if d.Code != tc.wantCode {
				t.Errorf("code: want %q, got %q", tc.wantCode, d.Code)
			}
			if d.Title != tc.wantTitle {
				t.Errorf("title: want %q, got %q", tc.wantTitle, d.Title)
			}
			if d.Type != tc.wantType {
				t.Errorf("type: want %q, got %q", tc.wantType, d.Type)
			}
			if d.Status != tc.status {
				t.Errorf("status: want %d, got %d", tc.status, d.Status)
			}
			if d.Detail != "test detail" {
				t.Errorf("detail: want %q, got %q", "test detail", d.Detail)
			}
		})
	}
}

// TestFrom_UnknownStatus ensures an unknown status code does NOT panic and returns a sensible fallback.
func TestFrom_UnknownStatus(t *testing.T) {
	t.Parallel()

	d := problem.From(599, "unknown status")
	if d == nil {
		t.Fatal("From returned nil for unknown status")
	}
	if d.Status != 599 {
		t.Errorf("status: want 599, got %d", d.Status)
	}
	// http.StatusText(599) is "", so From falls back to the generic "error" code and its derived type URI.
	// Pin both so a regression in the fallback branch is caught (a non-empty check would pass on garbage).
	if d.Code != "error" {
		t.Errorf("code: want %q, got %q", "error", d.Code)
	}
	if d.Type != "https://qovira.ai/errors/error" {
		t.Errorf("type: want %q, got %q", "https://qovira.ai/errors/error", d.Type)
	}
}

// ---------------------------------------------------------------------------
// code ↔ type slug round-trip.
// ---------------------------------------------------------------------------

func TestCodeSlugRoundTrip(t *testing.T) {
	t.Parallel()

	// Verify each constructor builds the correct type URI.
	cases := []struct {
		d    *problem.Details
		slug string
	}{
		{problem.Validation("x", nil), "validation-error"},
		{problem.NotFound("x"), "not-found"},
		{problem.MethodNotAllowed("x"), "method-not-allowed"},
		{problem.UnsupportedMediaType("x"), "unsupported-media-type"},
		{problem.Internal("x"), "internal-error"},
	}
	for _, c := range cases {
		wantType := "https://qovira.ai/errors/" + c.slug
		if c.d.Type != wantType {
			t.Errorf("%s: type: want %q, got %q", c.slug, wantType, c.d.Type)
		}
	}
}

// ---------------------------------------------------------------------------
// Details implements huma interfaces.
// ---------------------------------------------------------------------------

func TestDetails_HumaInterfaces(t *testing.T) {
	t.Parallel()

	d := problem.NotFound("resource not found")

	// GetStatus must return 404.
	if d.GetStatus() != http.StatusNotFound {
		t.Errorf("GetStatus: want 404, got %d", d.GetStatus())
	}

	// Error() returns Detail.
	if d.Error() != "resource not found" {
		t.Errorf("Error(): want %q, got %q", "resource not found", d.Error())
	}

	// ContentType filter: application/json → application/problem+json.
	got := d.ContentType("application/json")
	if got != "application/problem+json" {
		t.Errorf("ContentType(application/json): want %q, got %q", "application/problem+json", got)
	}

	// Other content types pass through.
	got = d.ContentType("text/plain")
	if got != "text/plain" {
		t.Errorf("ContentType(text/plain): want %q, got %q", "text/plain", got)
	}
}

// ---------------------------------------------------------------------------
// From converts huma.ErrorDetailer errs into FieldErrors.
// ---------------------------------------------------------------------------

func TestFrom_FieldErrors(t *testing.T) {
	t.Parallel()

	errs := []error{
		&huma.ErrorDetail{
			Message:  "expected required property name to be present",
			Location: "body.name",
			Value:    nil,
		},
		&huma.ErrorDetail{
			Message:  "expected length >= 3",
			Location: "body.items[0].title",
			Value:    "a",
		},
	}

	d := problem.From(http.StatusUnprocessableEntity, "validation failed", errs...)

	if len(d.Errors) != 2 {
		t.Fatalf("want 2 field errors, got %d", len(d.Errors))
	}

	if d.Errors[0].Pointer != "/name" {
		t.Errorf("errors[0].pointer: want %q, got %q", "/name", d.Errors[0].Pointer)
	}
	if d.Errors[0].Code != "required" {
		t.Errorf("errors[0].code: want %q, got %q", "required", d.Errors[0].Code)
	}

	if d.Errors[1].Pointer != "/items/0/title" {
		t.Errorf("errors[1].pointer: want %q, got %q", "/items/0/title", d.Errors[1].Pointer)
	}
	if d.Errors[1].Code != "min_length" {
		t.Errorf("errors[1].code: want %q, got %q", "min_length", d.Errors[1].Code)
	}
}

// ---------------------------------------------------------------------------
// WriteJSON emits the correct content-type and status.
// ---------------------------------------------------------------------------

func TestWriteJSON(t *testing.T) {
	t.Parallel()

	d := problem.NotFound("not found")
	d.RequestID = "req_TEST123"

	rr := httptest.NewRecorder()
	problem.WriteJSON(rr, d)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status: want 404, got %d", rr.Code)
	}
	ct := rr.Header().Get("Content-Type")
	if ct != "application/problem+json" {
		t.Errorf("Content-Type: want %q, got %q", "application/problem+json", ct)
	}

	var payload map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Verify all required fields are present.
	for _, field := range []string{"type", "title", "status", "detail", "code", "requestId"} {
		if _, ok := payload[field]; !ok {
			t.Errorf("missing field %q in problem+json output", field)
		}
	}

	// Verify no stray $schema field.
	if _, ok := payload["$schema"]; ok {
		t.Error("stray $schema field in problem+json output")
	}

	// Verify requestId matches.
	if payload["requestId"] != "req_TEST123" {
		t.Errorf("requestId: want %q, got %v", "req_TEST123", payload["requestId"])
	}
}

// ---------------------------------------------------------------------------
// Validation constructor populates errors array.
// ---------------------------------------------------------------------------

func TestValidation_WithFields(t *testing.T) {
	t.Parallel()

	fields := []problem.FieldError{
		{Pointer: "/email", Code: "format", Detail: "expected RFC 5322 email"},
	}
	d := problem.Validation("validation failed", fields)

	if d.Status != http.StatusUnprocessableEntity {
		t.Errorf("status: want 422, got %d", d.Status)
	}
	if len(d.Errors) != 1 {
		t.Fatalf("want 1 field error, got %d", len(d.Errors))
	}
	if d.Errors[0].Pointer != "/email" {
		t.Errorf("errors[0].pointer: want %q, got %q", "/email", d.Errors[0].Pointer)
	}
}
