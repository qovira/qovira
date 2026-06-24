package httpx_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qovira/qovira/internal/httpx"
)

// problemBody is the JSON shape used only in these tests to decode a problem+json response. Fields match the RFC 9457
// + house-extension contract.
type problemBody struct {
	Type      string           `json:"type"`
	Title     string           `json:"title"`
	Status    int              `json:"status"`
	Detail    string           `json:"detail"`
	Code      string           `json:"code"`
	RequestID string           `json:"requestId"`
	Errors    []fieldErrorBody `json:"errors"`
}

type fieldErrorBody struct {
	Pointer string `json:"pointer"`
	Detail  string `json:"detail"`
}

// writeProblem is a test helper that wires up an httptest.ResponseRecorder, calls WriteProblem, and decodes the JSON
// body.
func writeProblem(t *testing.T, r *http.Request, p httpx.Problem) (rr *httptest.ResponseRecorder, body problemBody) {
	t.Helper()
	rr = httptest.NewRecorder()
	httpx.WriteProblem(rr, r, p)
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("json.Decode problem body: %v", err)
	}
	return rr, body
}

// newGET returns a GET request against "/" with no context, for tests that do not need a specific request context.
func newGET(t *testing.T) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, "/", nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	return r
}

// TestWriteProblem_ContentType_And_RequiredFields verifies Acceptance Criterion 1: every response carries Content-Type:
// application/problem+json, a body with type/title/status/detail/code/requestId, and the HTTP status code matches the
// problem status field.
func TestWriteProblem_ContentType_And_RequiredFields(t *testing.T) {
	t.Parallel()

	p := httpx.Problem{
		Type:   "https://qovira.ai/errors/not-found",
		Title:  "Resource not found",
		Status: http.StatusNotFound,
		Detail: "No item with that id.",
		Code:   "not_found",
	}

	r := newGET(t)
	rr, body := writeProblem(t, r, p)

	if got := rr.Code; got != http.StatusNotFound {
		t.Errorf("HTTP status = %d, want %d", got, http.StatusNotFound)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/problem+json")
	}

	if body.Type == "" {
		t.Error("body.type is empty")
	}
	if body.Title == "" {
		t.Error("body.title is empty")
	}
	if body.Status != http.StatusNotFound {
		t.Errorf("body.status = %d, want %d", body.Status, http.StatusNotFound)
	}
	if body.Detail == "" {
		t.Error("body.detail is empty")
	}
	if body.Code == "" {
		t.Error("body.code is empty")
	}
	if body.RequestID == "" {
		t.Error("body.requestId is empty (expected a generated placeholder)")
	}
}

// TestWriteProblem_RequestIDFromContext verifies that WriteProblem populates requestId from the context value set by
// ContextWithRequestID / RequestIDFromContext (Acceptance Criterion 5, requestId sub-criterion).
func TestWriteProblem_RequestIDFromContext(t *testing.T) {
	t.Parallel()

	const wantID = "req_test_abc123"

	r := newGET(t)
	r = r.WithContext(httpx.ContextWithRequestID(r.Context(), wantID))

	p := httpx.Problem{
		Type:   "https://qovira.ai/errors/not-found",
		Title:  "Resource not found",
		Status: http.StatusNotFound,
		Detail: "No item with that id.",
		Code:   "not_found",
	}
	_, body := writeProblem(t, r, p)

	if body.RequestID != wantID {
		t.Errorf("requestId = %q, want %q", body.RequestID, wantID)
	}
}

// TestValidationProblem verifies Acceptance Criterion 2: a 422 response with an errors[] array locating each offending
// field by JSON Pointer.
func TestValidationProblem_Status_And_Pointers(t *testing.T) {
	t.Parallel()

	fields := []httpx.FieldError{
		{Pointer: "/email", Detail: "must be a valid email"},
		{Pointer: "/items/0/name", Detail: "required"},
	}
	p := httpx.ValidationProblem("validation_error", "The request has invalid fields.", fields...)

	r := newGET(t)
	rr, body := writeProblem(t, r, p)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("HTTP status = %d, want 422", rr.Code)
	}
	if body.Status != http.StatusUnprocessableEntity {
		t.Errorf("body.status = %d, want 422", body.Status)
	}
	if len(body.Errors) != 2 {
		t.Fatalf("len(body.errors) = %d, want 2", len(body.Errors))
	}
	if body.Errors[0].Pointer != "/email" {
		t.Errorf("errors[0].pointer = %q, want %q", body.Errors[0].Pointer, "/email")
	}
	if body.Errors[1].Pointer != "/items/0/name" {
		t.Errorf("errors[1].pointer = %q, want %q", body.Errors[1].Pointer, "/items/0/name")
	}
}

// TestMalformedBodyProblem verifies Acceptance Criterion 3: a 400 response via the MalformedBodyProblem helper.
func TestMalformedBodyProblem_Status400(t *testing.T) {
	t.Parallel()

	p := httpx.MalformedBodyProblem()

	r := newGET(t)
	rr, body := writeProblem(t, r, p)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("HTTP status = %d, want 400", rr.Code)
	}
	if body.Status != http.StatusBadRequest {
		t.Errorf("body.status = %d, want 400", body.Status)
	}
	if body.Code == "" {
		t.Error("body.code is empty")
	}
}

// TestInternalProblem_NoInternalLeak verifies Acceptance Criterion 4: a 5xx response body must NOT contain the internal
// error string (stack trace, SQL, hostname, etc.). It also proves the internal message WAS logged.
func TestInternalProblem_NoInternalLeak(t *testing.T) {
	t.Parallel()

	// internalErrMsg is the raw error text that must reach the log but must never appear in the HTTP response body. Use
	// characters safe for JSON (no quotes) so strings.Contains works on the raw log output.
	const internalErrMsg = "pq: syntax error near SELECT -- internal-hostname-db01"

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	p := httpx.InternalProblem(logger, "internal_error", internalErrMsg)

	r := newGET(t)
	rr, body := writeProblem(t, r, p)

	// HTTP status must be 500.
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("HTTP status = %d, want 500", rr.Code)
	}

	// The raw response body must NOT contain the internal error text.
	rawBody := rr.Body.String()
	if strings.Contains(rawBody, internalErrMsg) {
		t.Errorf("response body contains internal error string: %s", rawBody)
	}
	if strings.Contains(rawBody, "db01") {
		t.Error("response body leaks internal hostname")
	}

	// The internal error string MUST have been logged.
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, internalErrMsg) {
		t.Errorf("internal error not logged; log output: %s", logOutput)
	}

	// Body fields: code and requestId must be present; detail must be generic.
	if body.Code == "" {
		t.Error("body.code is empty")
	}
	if body.RequestID == "" {
		t.Error("body.requestId is empty")
	}
}

// TestTypeURL verifies Acceptance Criterion 5: the type URL is rooted at https://qovira.ai/errors/{slug} when no
// explicit Type is provided.
func TestTypeURL_DerivedFromCode(t *testing.T) {
	t.Parallel()

	p := httpx.Problem{
		// Type intentionally empty — WriteProblem must derive it from Code.
		Title:  "Resource not found",
		Status: http.StatusNotFound,
		Detail: "No item with that id.",
		Code:   "not_found",
	}

	r := newGET(t)
	_, body := writeProblem(t, r, p)

	want := httpx.ErrorTypeBase + "not_found"
	if body.Type != want {
		t.Errorf("body.type = %q, want %q", body.Type, want)
	}
}

// TestTypeURL_ExplicitTypePreserved verifies that an explicitly set Type is not overwritten by the slug-derivation
// logic.
func TestTypeURL_ExplicitTypePreserved(t *testing.T) {
	t.Parallel()

	const explicit = "https://custom.example.com/errors/custom-type"
	p := httpx.Problem{
		Type:   explicit,
		Title:  "Custom",
		Status: http.StatusNotFound,
		Detail: "detail",
		Code:   "custom",
	}

	r := newGET(t)
	_, body := writeProblem(t, r, p)

	if body.Type != explicit {
		t.Errorf("body.type = %q, want explicit %q", body.Type, explicit)
	}
}

// TestContextWithRequestID_RoundTrip verifies that ContextWithRequestID and RequestIDFromContext form a consistent
// get/set pair.
func TestContextWithRequestID_RoundTrip(t *testing.T) {
	t.Parallel()

	r := newGET(t)
	if got := httpx.RequestIDFromContext(r.Context()); got != "" {
		t.Errorf("empty context: RequestIDFromContext = %q, want empty string", got)
	}

	const wantID = "req_roundtrip_42"
	ctx := httpx.ContextWithRequestID(r.Context(), wantID)
	if got := httpx.RequestIDFromContext(ctx); got != wantID {
		t.Errorf("RequestIDFromContext = %q, want %q", got, wantID)
	}
}

// TestValidationProblem_TypeURL verifies that ValidationProblem sets the type URL correctly from the code slug.
func TestValidationProblem_TypeURL(t *testing.T) {
	t.Parallel()

	p := httpx.ValidationProblem("validation_error", "details")
	r := newGET(t)
	_, body := writeProblem(t, r, p)

	want := httpx.ErrorTypeBase + "validation_error"
	if body.Type != want {
		t.Errorf("body.type = %q, want %q", body.Type, want)
	}
}

// TestInternalProblem_StatusCode verifies that InternalProblem always returns HTTP 500 regardless of what code is
// passed.
func TestInternalProblem_StatusCode(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	p := httpx.InternalProblem(logger, "some_internal_code", "connection refused")
	r := newGET(t)
	rr, _ := writeProblem(t, r, p)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("HTTP status = %d, want 500", rr.Code)
	}
}

// TestWriteProblem_ZeroStatusDefaultsTo500 verifies that WriteProblem guards against a zero/unset Status field: a
// caller that constructs a Problem without setting Status must not emit a 200 with a problem body. The guard defaults
// to 500 Internal Server Error.
func TestWriteProblem_ZeroStatusDefaultsTo500(t *testing.T) {
	t.Parallel()

	p := httpx.Problem{
		Title:  "Something went wrong",
		Detail: "no status was set",
		Code:   "internal_error",
		// Status: 0  ← intentionally omitted
	}

	r := newGET(t)
	rr, body := writeProblem(t, r, p)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("HTTP status = %d, want 500 (zero Status must default to 500)", rr.Code)
	}
	if body.Status != http.StatusInternalServerError {
		t.Errorf("body.status = %d, want 500", body.Status)
	}
}

// TestWriteProblem_NonZeroStatusUnchanged verifies that an explicitly set non-zero Status is not clobbered by the
// zero-Status guard.
func TestWriteProblem_NonZeroStatusUnchanged(t *testing.T) {
	t.Parallel()

	p := httpx.Problem{
		Title:  "Resource not found",
		Status: http.StatusNotFound,
		Detail: "no item",
		Code:   "not_found",
	}

	r := newGET(t)
	rr, body := writeProblem(t, r, p)

	if rr.Code != http.StatusNotFound {
		t.Errorf("HTTP status = %d, want 404", rr.Code)
	}
	if body.Status != http.StatusNotFound {
		t.Errorf("body.status = %d, want 404", body.Status)
	}
}
