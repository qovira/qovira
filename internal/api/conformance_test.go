package api_test

// conformance_test.go — contract-conformance tests that boot the real API over httptest and validate live
// responses against the committed OpenAPI 3.1 spec using libopenapi-validator.
//
// Design goals:
//   - Generic operation loop: operations are enumerated from the spec model, not hardcoded, so new endpoints
//     are covered for free.
//   - Server-prefix awareness: the spec has servers: [{url: /api/v1}]; libopenapi-validator strips that
//     prefix when matching path keys, so the live request must use the full /api/v1/... path.
//   - Error-body contract: undocumented fallback paths (404) are validated against the Details component
//     schema extracted from the spec via the schema_validation sub-package.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
	"github.com/pb33f/libopenapi-validator/schema_validation"
	"github.com/pb33f/libopenapi/orderedmap"
)

// fetchSpec fetches the YAML spec bytes from /api/v1/openapi.yaml on the given test server. Using the
// served endpoint (rather than reading openapi.yaml from disk) simultaneously exercises the spec endpoint
// and guarantees the validator is working against the same bytes the server will serve.
func fetchSpec(t *testing.T, srv *httptest.Server) []byte {
	t.Helper()

	resp, err := http.Get(srv.URL + "/api/v1/openapi.yaml") //nolint:noctx // test-only convenience
	if err != nil {
		t.Fatalf("fetch spec: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fetch spec: want 200, got %d", resp.StatusCode)
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read spec bytes: %v", err)
	}

	return b
}

// TestConformance_OperationResponses boots the real API, enumerates every registered operation from the
// parsed spec model (so new endpoints are covered for free), issues a live request for each, and validates
// the response against the OpenAPI contract via libopenapi-validator.
//
// Server-prefix: the spec declares `servers: [{url: /api/v1}]`. libopenapi-validator's FindPath calls
// StripRequestPath which strips /api/v1 before matching path keys — so the live request URL must carry the
// full /api/v1/<path> (which it does naturally when talking to the test server), no manual workaround needed.
func TestConformance_OperationResponses(t *testing.T) {
	t.Parallel()

	srv := newTestMux(t)
	t.Cleanup(srv.Close)

	// Fetch the spec from the live server; this also verifies the /api/v1/openapi.yaml endpoint.
	specBytes := fetchSpec(t, srv)

	doc, err := libopenapi.NewDocument(specBytes)
	if err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	v3Model, err := doc.BuildV3Model()
	if err != nil {
		t.Fatalf("build v3 model: %v", err)
	}

	docValidator, validatorErrs := validator.NewValidator(doc)
	if len(validatorErrs) > 0 {
		t.Fatalf("build validator: %v", validatorErrs)
	}
	t.Cleanup(docValidator.Release)

	if v3Model.Model.Paths == nil || v3Model.Model.Paths.PathItems == nil {
		t.Fatal("spec has no paths — nothing to validate")
	}

	// Build the operation list generically from the spec model. New handlers auto-appear here when
	// registered, so the conformance test scales without manual updates.
	type operation struct {
		method   string // e.g. "GET"
		specPath string // spec key, e.g. "/health" (no server prefix)
		livePath string // full request path, e.g. "/api/v1/health"
	}

	const serverPrefix = "/api/v1"
	var ops []operation

	for pathPair := orderedmap.First(v3Model.Model.Paths.PathItems); pathPair != nil; pathPair = pathPair.Next() {
		specPath := pathPair.Key()
		pathItem := pathPair.Value()

		for methodPair := pathItem.GetOperations().First(); methodPair != nil; methodPair = methodPair.Next() {
			ops = append(ops, operation{
				method:   strings.ToUpper(methodPair.Key()),
				specPath: specPath,
				livePath: serverPrefix + specPath,
			})
		}
	}

	if len(ops) == 0 {
		t.Fatal("no operations found in spec — nothing to validate")
	}

	for _, op := range ops {
		t.Run(fmt.Sprintf("%s %s", op.method, op.specPath), func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest(op.method, srv.URL+op.livePath, nil)
			if err != nil {
				t.Fatalf("new request %s %s: %v", op.method, op.livePath, err)
			}

			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("do request %s %s: %v", op.method, op.livePath, err)
			}
			defer resp.Body.Close()

			// ValidateHttpResponse resolves the operation from the full /api/v1/... request URL,
			// strips the server prefix internally, and validates the response body + headers.
			valid, validationErrs := docValidator.ValidateHttpResponse(req, resp)
			if !valid {
				var sb strings.Builder
				for _, e := range validationErrs {
					fmt.Fprintf(&sb, "\n  - [%s] %s", e.ValidationType, e.Message)
					for _, se := range e.SchemaValidationErrors {
						fmt.Fprintf(&sb, "\n      schema: %s (line %d col %d)", se.Reason, se.Line, se.Column)
					}
				}

				t.Errorf("%s %s response violated spec contract:%s", op.method, op.livePath, sb.String())
			}
		})
	}
}

// TestConformance_ErrorBodySchema triggers a 404 on an unknown /api/v1 path and validates the response
// body against the Details component schema extracted from the spec. The fallback-generated 404 is not
// documented in the paths object, so ValidateHttpResponse cannot map it to an operation — we validate
// the raw JSON body directly via schema_validation.SchemaValidator instead.
func TestConformance_ErrorBodySchema(t *testing.T) {
	t.Parallel()

	srv := newTestMux(t)
	t.Cleanup(srv.Close)

	specBytes := fetchSpec(t, srv)

	doc, err := libopenapi.NewDocument(specBytes)
	if err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	v3Model, err := doc.BuildV3Model()
	if err != nil {
		t.Fatalf("build v3 model: %v", err)
	}

	if v3Model.Model.Components == nil || v3Model.Model.Components.Schemas == nil {
		t.Fatal("spec has no components/schemas")
	}

	detailsProxy, ok := v3Model.Model.Components.Schemas.Get("Details")
	if !ok {
		t.Fatal("spec missing Details component schema")
	}

	detailsSchema := detailsProxy.Schema()
	if detailsSchema == nil {
		t.Fatalf("Details schema build error: %v", detailsProxy.GetBuildError())
	}

	// Trigger a 404 from the fallback — the path is deliberately unknown.
	resp, err := srv.Client().Get(srv.URL + "/api/v1/does-not-exist-conformance")
	if err != nil {
		t.Fatalf("GET /api/v1/does-not-exist-conformance: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}

	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type: want application/problem+json, got %q", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	// Decode once to a Go value; the schema validator accepts the pre-decoded object directly.
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", err, body)
	}

	// Validate the decoded 404 body against the Details component schema from the spec.
	schemaValidator := schema_validation.NewSchemaValidator()
	t.Cleanup(schemaValidator.Release)

	valid, validationErrs := schemaValidator.ValidateSchemaObject(detailsSchema, raw)
	if !valid {
		var sb strings.Builder
		for _, e := range validationErrs {
			fmt.Fprintf(&sb, "\n  - %s", e.Message)
			for _, se := range e.SchemaValidationErrors {
				fmt.Fprintf(&sb, "\n      %s (line %d col %d)", se.Reason, se.Line, se.Column)
			}
		}

		t.Errorf("404 problem+json body does not conform to Details schema:%s\nbody: %s", sb.String(), body)
	}
}
