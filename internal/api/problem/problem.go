// Package problem provides the house RFC 9457 problem+json shape and the machinery that makes every Huma
// error emerge in it. It is a pure rendering package: it imports huma for the ErrorDetailer interface but
// has no dependency on httpx or the composition root, so it can be tested in isolation.
//
// The package installs a process-global huma.NewError override in its init() function so that every error
// produced by Huma — validation (422), unsupported media type (415), panics (500), and any future codes —
// emerges as a *Details carrying the house extensions (code, requestId, type URI) with zero per-handler work.
// Importing this package is a side-effecting operation: any huma.API built anywhere in the process will
// render errors as *Details. This is intentional — there is exactly one API — but callers should be aware.
//
// The requestId field is left empty by From and filled in by the response transformer registered in
// internal/api, which reads the ID from the request context via httpx.RequestID.
package problem

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// typeBase is the base URI for house error type URIs. Each error kind is identified by
// https://qovira.ai/errors/{slug}, where {slug} is the kebab-case form of the error code.
const typeBase = "https://qovira.ai/errors/"

// Details is the house RFC 9457 problem+json body. It implements huma.StatusError (GetStatus, Error),
// huma.ContentTypeFilter (ContentType), and can be marshaled to JSON with the required field set.
//
// The json tags are authoritative: they match the AC-verified wire shape exactly. Fields marked
// omitempty are optional per RFC 9457 (instance, errors).
type Details struct {
	Type      string       `json:"type"`
	Title     string       `json:"title"`
	Status    int          `json:"status"`
	Detail    string       `json:"detail"`
	Instance  string       `json:"instance,omitempty"`
	Code      string       `json:"code"`
	RequestID string       `json:"requestId"`
	Errors    []FieldError `json:"errors,omitempty"`
}

// FieldError is a single per-field validation error in the errors array.
type FieldError struct {
	// Pointer is an RFC 6901 JSON Pointer locating the invalid field within the request body (e.g. /items/0/name).
	Pointer string `json:"pointer"`
	// Code is the house per-field error code (required, min, max, min_length, max_length, format, enum, pattern,
	// type, invalid, …).
	Code string `json:"code"`
	// Detail is the human-readable per-field message (the raw Huma validation message).
	Detail string `json:"detail"`
}

// Error implements the error interface.
func (d *Details) Error() string {
	return d.Detail
}

// GetStatus implements huma.StatusError.
func (d *Details) GetStatus() int {
	return d.Status
}

// ContentType implements huma.ContentTypeFilter. When Huma would set application/json, we override it to
// application/problem+json as required by RFC 9457.
func (d *Details) ContentType(ct string) string {
	if ct == "application/json" {
		return "application/problem+json"
	}
	return ct
}

// kind holds the static fields for a single error code in the registry.
type kind struct {
	code   string
	title  string
	status int
}

// typeURI derives the type URI for a code by converting underscores to hyphens.
// "validation_error" → "https://qovira.ai/errors/validation-error".
func typeURI(code string) string {
	return typeBase + strings.ReplaceAll(code, "_", "-")
}

// registry maps HTTP status codes to their house error kind. Only the five framework codes are registered;
// From falls back gracefully for unknown codes.
var registry = map[int]kind{
	http.StatusUnprocessableEntity: {code: "validation_error", title: "Validation Error", status: http.StatusUnprocessableEntity},
	http.StatusNotFound:            {code: "not_found", title: "Not Found", status: http.StatusNotFound},
	http.StatusMethodNotAllowed:    {code: "method_not_allowed", title: "Method Not Allowed", status: http.StatusMethodNotAllowed},
	http.StatusUnsupportedMediaType: {code: "unsupported_media_type", title: "Unsupported Media Type",
		status: http.StatusUnsupportedMediaType},
	http.StatusInternalServerError: {code: "internal_error", title: "Internal Error", status: http.StatusInternalServerError},
}

// From builds a *Details from the given HTTP status code and message. It looks up the registry for the
// house code/title/type triple; unknown statuses get a sensible generic fallback (never panic). Each err
// that implements huma.ErrorDetailer is converted into a FieldError with locationToPointer + messageToCode.
// The RequestID field is left empty — it is filled by the response transformer in internal/api.
func From(status int, msg string, errs ...error) *Details {
	k, ok := registry[status]
	if !ok {
		// Fallback: derive code from http.StatusText (snake-cased, lower-cased).
		text := http.StatusText(status)
		if text == "" {
			text = "error"
		}
		code := strings.ReplaceAll(strings.ToLower(text), " ", "_")
		k = kind{code: code, title: text, status: status}
	}

	d := &Details{
		Type:   typeURI(k.code),
		Title:  k.title,
		Status: status,
		Detail: msg,
		Code:   k.code,
	}

	for _, e := range errs {
		if e == nil {
			continue
		}
		if de, ok := e.(huma.ErrorDetailer); ok {
			ed := de.ErrorDetail()
			d.Errors = append(d.Errors, FieldError{
				Pointer: locationToPointer(ed.Location),
				Code:    messageToCode(ed.Message),
				Detail:  ed.Message,
			})
		}
	}

	return d
}

// NotFound builds a 404 *Details.
func NotFound(detail string) *Details {
	return From(http.StatusNotFound, detail)
}

// MethodNotAllowed builds a 405 *Details.
func MethodNotAllowed(detail string) *Details {
	return From(http.StatusMethodNotAllowed, detail)
}

// UnsupportedMediaType builds a 415 *Details.
func UnsupportedMediaType(detail string) *Details {
	return From(http.StatusUnsupportedMediaType, detail)
}

// Validation builds a 422 *Details with the given pre-classified FieldErrors. Use From with huma error
// details when the errors come from Huma's validation pipeline; use this constructor when the errors are
// already classified (e.g. from a test, or a domain layer that pre-classifies them).
func Validation(detail string, fields []FieldError) *Details {
	d := From(http.StatusUnprocessableEntity, detail)
	d.Errors = fields
	return d
}

// Internal builds a 500 *Details.
func Internal(detail string) *Details {
	return From(http.StatusInternalServerError, detail)
}

// WriteJSON writes d as an application/problem+json response to w. Callers must set any extra headers
// (e.g. Allow for 405) on w BEFORE calling WriteJSON, because WriteHeader flushes the header map.
func WriteJSON(w http.ResponseWriter, d *Details) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(d.Status)

	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(d)
}

// locationToPointer converts a Huma validation-error location ({prefix}.{rest}, prefix ∈ body/path/query/
// header/cookie, rest using dot fields and [i] indices) into an RFC 6901 JSON Pointer. The full case table
// lives in problem_test.go; the non-obvious bits:
//
//	"body.items[0].dueDate" → "/items/0/dueDate"   (fields and indices both become tokens)
//	"body"                  → ""                     (whole-body error)
//	"body.a~b"              → "/a~0b"                (RFC 6901: ~ and / escaped)
//	"query.limit"           → "/limit"              (non-body: strip prefix best-effort)
func locationToPointer(location string) string {
	if location == "" {
		return ""
	}

	// Strip the leading segment (body / query / path / header / cookie) and the dot.
	// strings.Cut splits on the first '.'; if there is none, the location is a bare prefix
	// ("body", "query", …) with no field path → whole-resource pointer.
	_, rest, ok := strings.Cut(location, ".")
	if !ok {
		return ""
	}

	// Convert the Huma dot+bracket path to an RFC 6901 pointer, one token at a time (split on '.', '[', ']').
	var buf strings.Builder

	var token strings.Builder

	flushToken := func() {
		s := token.String()
		if s == "" {
			return
		}
		// Apply RFC 6901 escaping: ~ first (to avoid double-escaping), then /.
		s = strings.ReplaceAll(s, "~", "~0")
		s = strings.ReplaceAll(s, "/", "~1")
		buf.WriteByte('/')
		buf.WriteString(s)
		token.Reset()
	}

	for _, ch := range rest {
		switch ch {
		case '.':
			flushToken()
		case '[':
			flushToken()
		case ']':
			flushToken()
		default:
			token.WriteRune(ch)
		}
	}
	flushToken()

	return buf.String()
}

// messageToCode maps a Huma validation message (as emitted by validation/messages.go in v2.38.0) to a house
// per-field code by prefix-matching. The switch order is load-bearing: isFormatMessage is checked before the
// generic "expected string to be" pattern guard, and unmatched messages fall through to "invalid".
func messageToCode(message string) string {
	switch {
	case strings.HasPrefix(message, "expected required property"):
		return "required"
	case strings.HasPrefix(message, "expected length >="):
		return "min_length"
	case strings.HasPrefix(message, "expected length <="):
		return "max_length"
	case strings.HasPrefix(message, "expected number >="),
		strings.HasPrefix(message, "expected number >"):
		return "min"
	case strings.HasPrefix(message, "expected number <="),
		strings.HasPrefix(message, "expected number <"):
		return "max"
	case strings.HasPrefix(message, "expected array length >="):
		return "min"
	case strings.HasPrefix(message, "expected array length <="):
		return "max"
	case isFormatMessage(message):
		return "format"
	case strings.HasPrefix(message, "expected string to match pattern"),
		strings.HasPrefix(message, "expected string to be "):
		// Only reached when isFormatMessage returned false, so the suffix is a literal pattern
		// description (MsgExpectedBePattern / MsgExpectedMatchPattern).
		return "pattern"
	case strings.HasPrefix(message, "expected value to be one of"):
		return "enum"
	case message == "expected boolean",
		message == "expected number",
		message == "expected integer",
		message == "expected string",
		message == "expected array",
		message == "expected object":
		return "type"
	default:
		return "invalid"
	}
}

// isFormatMessage reports whether msg is one of Huma's format-validation messages (all share the
// "expected string to be " prefix, but differ in their suffix). Recognised suffixes, from messages.go:
//
//   - "RFC …"     — any RFC-numbered format (date-time, email, uuid, hostname, ipv4, ipv6, uri, …)
//   - "either …"  — MsgExpectedRFCIPAddr ("expected string to be either RFC 2673 ipv4 or RFC 2373 ipv6")
//   - "base64 …"  — MsgExpectedBase64String
//   - "regex: …"  — MsgExpectedRegexp ("expected string to be regex: <err>")
//
// Literal pattern descriptions (MsgExpectedBePattern: "expected string to be <pattern>") do NOT match
// any of those prefixes, so they correctly fall through to the pattern case in messageToCode.
func isFormatMessage(msg string) bool {
	const prefix = "expected string to be "
	rest, ok := strings.CutPrefix(msg, prefix)
	if !ok {
		return false
	}
	return strings.HasPrefix(rest, "RFC ") ||
		strings.HasPrefix(rest, "either ") ||
		strings.HasPrefix(rest, "base64") ||
		strings.HasPrefix(rest, "regex:")
}

// init sets huma.NewError to produce *Details so that every error Huma generates — validation (422),
// unsupported media type (415), panics (500), and any future codes — emerges in the house problem+json
// shape. This is done in init() (set once at program load) rather than in api.New, because tests call
// api.New concurrently and mutating a package-level var from multiple goroutines is a data race.
func init() {
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		return From(status, msg, errs...)
	}
}
