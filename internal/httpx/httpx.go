// Package httpx provides the HTTP server, router, middleware, problem+json responses, and the /events endpoint. (Built
// up across the HTTP slices.)
//
// This file implements the RFC 9457 problem+json error helper:
//   - Problem, FieldError types with camelCase JSON marshaling.
//   - WriteProblem: writes a problem+json response, derives the type URL from the code slug when Type is empty, and
//     fills requestId from context.
//   - ValidationProblem, MalformedBodyProblem, InternalProblem constructors.
//   - ContextWithRequestID / RequestIDFromContext: request-id context wiring. The request-id middleware that populates
//     the context lands separately; only the accessor/setter pair lives here so the middleware can plug in.
package httpx

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
)

// ErrorTypeBase is the stable URL prefix for all Qovira problem types. The full type URL for a given code slug is
// ErrorTypeBase + slug, e.g. "https://qovira.ai/errors/validation_error". This constant is exported so tests and
// callers can construct expected values without string duplication.
const ErrorTypeBase = "https://qovira.ai/errors/"

// unknownRequestID is the placeholder written into a Problem's requestId when no correlation ID is present in the
// request context, so the field is never absent. The request-id middleware and log correlation share this sentinel.
const unknownRequestID = "unknown"

// contextKey is the unexported type for all context keys owned by this package. Using a dedicated type prevents
// collisions with keys from other packages.
type contextKey int

// All context keys owned by this package are declared together in this single iota block so their values stay distinct
// by construction. Adding a key here — never with a hand-written iota offset in another file — is what prevents two
// keys from colliding on the same underlying value.
const (
	// requestIDKey is the context key for the request correlation ID.
	requestIDKey contextKey = iota
	// principalKey is the context key for the authenticated store.Principal.
	principalKey
)

// ContextWithRequestID returns a new context carrying the given request ID. The value is retrieved by
// RequestIDFromContext and consumed by WriteProblem. The request-id middleware calls this to inject the ID; call sites
// that synthesise a request should also call it.
func ContextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFromContext retrieves the request ID stored by ContextWithRequestID. Returns an empty string when no ID has
// been set.
func RequestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

// FieldError locates a single invalid value within a request body using an RFC 6901 JSON Pointer (e.g. "/email" or
// "/items/0/name"). An array of FieldErrors is embedded in a Problem to form a validation error response.
type FieldError struct {
	// Pointer is the RFC 6901 JSON Pointer to the offending field.
	Pointer string `json:"pointer"`
	// Detail is a human-readable explanation of why the value is invalid.
	Detail string `json:"detail"`
}

// Problem is an RFC 9457 problem+json object. JSON field names are camelCase per the Qovira HTTP guide. Optional fields
// use omitempty; the required fields (Type, Title, Status, Detail, Code, RequestID) are always present in a marshaled
// response.
//
// Callers build a Problem via the constructors below (ValidationProblem, MalformedBodyProblem, InternalProblem) or by
// filling the struct directly for ad-hoc error shapes. WriteProblem finalises the Type and RequestID fields before
// writing.
type Problem struct {
	// Type is a stable URL identifying the problem kind. When empty, WriteProblem derives it from Code via
	// ErrorTypeBase.
	Type string `json:"type"`
	// Title is a short, stable, human-readable summary of the problem type.
	Title string `json:"title"`
	// Status is the HTTP status code; it must equal the response status line.
	Status int `json:"status"`
	// Detail is a human-readable explanation specific to this occurrence. Must never contain secrets, stack traces,
	// SQL, or internal hostnames.
	Detail string `json:"detail"`
	// Code is a short snake_case machine slug that clients branch on. It is stable and documented.
	Code string `json:"code"`
	// RequestID is the correlation ID tying this response to server logs. WriteProblem fills this from context
	// when it is empty.
	RequestID string `json:"requestId"`
	// Errors is an optional array of per-field validation errors. Present only on 422 validation responses; omitted
	// otherwise.
	Errors []FieldError `json:"errors,omitempty"`
}

// WriteProblem finalises p and writes it as an RFC 9457 problem+json response.
//
// Before writing, WriteProblem:
//  1. Fills p.RequestID from the request context (via RequestIDFromContext) if p.RequestID is empty. If the context
//     carries no ID either, a placeholder is used so the field is never absent.
//  2. Derives p.Type from p.Code as ErrorTypeBase+code when p.Type is empty.
//
// The response sets Content-Type: application/problem+json and the status code p.Status. JSON encoding and write errors
// are logged to the default slog logger; they cannot be surfaced to the client at that point.
func WriteProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	// 0. Guard against a zero/unset Status: a caller that forgets to set Status
	//    must not emit a 200 with a problem body. Default to 500 so the response
	//    is at least semantically correct.
	if p.Status == 0 {
		p.Status = http.StatusInternalServerError
	}

	// 1. Fill requestId from context if not already set.
	if p.RequestID == "" {
		p.RequestID = RequestIDFromContext(r.Context())
	}
	// Ensure requestId is never absent in the JSON body.
	if p.RequestID == "" {
		p.RequestID = unknownRequestID
	}

	// 2. Derive type URL from code slug when no explicit type was given.
	if p.Type == "" {
		p.Type = ErrorTypeBase + p.Code
	}

	body, err := json.Marshal(p)
	if err != nil {
		// Encoding should not fail for this well-typed struct. Log and fall back to a minimal static response so the
		// client gets something.
		slog.Error("httpx: failed to marshal problem", "err", err)
		http.Error(w, `{"type":"about:blank","title":"Internal Server Error","status":500}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)

	if _, err = w.Write(body); err != nil {
		slog.Error("httpx: failed to write problem body", "err", err)
	}
}

// internalServerProblem returns the canonical 500 problem+json body used whenever the server encounters an unexpected
// internal error. The generic detail and static code mean neither a stack trace nor internal context leaks to the
// client. Use InternalProblem (which calls this) when you also want to log the internal error.
func internalServerProblem() Problem {
	return Problem{
		Title:  "Internal server error",
		Status: http.StatusInternalServerError,
		Detail: "An unexpected error occurred. Quote the requestId when contacting support.",
		Code:   "internal_error",
	}
}

// ValidationProblem returns a 422 Problem populated with the given code slug, detail string, and zero or more
// FieldErrors. Each FieldError locates an offending value by JSON Pointer (RFC 6901), e.g. "/email".
//
// The type URL is derived from code via ErrorTypeBase.
func ValidationProblem(code, detail string, fields ...FieldError) Problem {
	return Problem{
		Title:  "Request validation failed",
		Status: http.StatusUnprocessableEntity,
		Detail: detail,
		Code:   code,
		Errors: fields,
	}
}

// MalformedBodyProblem returns a 400 Problem signalling that the request body could not be parsed — malformed JSON,
// wrong wire-level types, or a missing required body. Use this when the body never even validated.
func MalformedBodyProblem() Problem {
	return Problem{
		Title:  "Malformed request body",
		Status: http.StatusBadRequest,
		Detail: "The request body could not be parsed. Ensure it is valid JSON.",
		Code:   "malformed_body",
	}
}

// InternalProblem returns a 500 Problem and logs the internal error via logger. The internal error string is never
// placed in the response body — only a generic detail, the code slug, and the requestId are returned to the caller.
//
// code is a stable snake_case slug documented in the error registry. internalErr is the raw error message or detail
// that MUST NOT leave the server; it is recorded only in the structured log at Error level.
func InternalProblem(logger *slog.Logger, code string, internalErr string) Problem {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("internal error", "code", code, "err", internalErr)
	p := internalServerProblem()
	p.Code = code // callers may supply a more specific slug
	return p
}
