package gateway

import (
	"cmp"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors for gateway call outcomes.  All are comparable via [errors.Is]. Callers inspect the kind (via
// errors.Is) and, for [ErrRateLimited], cast to [*RateLimitedError] via [errors.As] to retrieve the optional retry
// delay.

// ErrAuth is returned when the upstream rejects the request with a 401 or 403, indicating a bad or missing API key.
var ErrAuth = errors.New("gateway: authentication error")

// ErrModelNotFound is returned when the configured model is not served by the upstream (typically a 404 with a
// model-not-found body).
var ErrModelNotFound = errors.New("gateway: model not found")

// ErrRateLimited is the sentinel matched by [errors.Is] for 429 responses. Wrap a [*RateLimitedError] in your call
// sites and use [errors.As] to retrieve the optional retry delay.
var ErrRateLimited = errors.New("gateway: rate limited")

// ErrContextLength is returned when the upstream signals that the input exceeds the model's context window.  It is kept
// distinct from generic 4xx errors so callers can trim the prompt and retry rather than surfacing a generic failure.
var ErrContextLength = errors.New("gateway: context length exceeded")

// ErrUpstream is the fallback for 5xx responses and network-level failures whose cause is not more specifically
// classified.
var ErrUpstream = errors.New("gateway: upstream error")

// ErrTimeout is returned when a first-token or idle timeout fires before the upstream delivers a complete response.
var ErrTimeout = errors.New("gateway: timeout")

// ErrUpstreamProtocol is returned when the upstream's response cannot be parsed (malformed SSE stream, unexpected
// content type, truncated JSON, etc.).
var ErrUpstreamProtocol = errors.New("gateway: upstream protocol error")

// ── RateLimitedError ──────────────────────────────────────────────────────────

// RateLimitedError wraps [ErrRateLimited] and optionally carries the parsed Retry-After delay when the upstream
// included that header.
//
// Use [errors.Is](err, [ErrRateLimited]) to check whether an error is a rate limit, and [errors.As] to retrieve the
// delay:
//
//	var rle *gateway.RateLimitedError
//	if errors.As(err, &rle) && rle.RetryAfter != nil {
//	    time.Sleep(*rle.RetryAfter)
//	}
type RateLimitedError struct {
	// RetryAfter is the parsed retry delay.  It is nil when the upstream did not include a Retry-After header or the
	// header value could not be parsed.
	RetryAfter *time.Duration
}

// Error implements the error interface.
func (e *RateLimitedError) Error() string {
	if e.RetryAfter != nil {
		return "gateway: rate limited (retry after " + e.RetryAfter.String() + ")"
	}
	return "gateway: rate limited"
}

// Unwrap returns [ErrRateLimited] so that [errors.Is](err, ErrRateLimited) works on any *[RateLimitedError].
func (e *RateLimitedError) Unwrap() error { return ErrRateLimited }

// ── Classifier ────────────────────────────────────────────────────────────────

// ClassifyResponse maps an HTTP status code plus the response headers and body to exactly one of the package-level
// sentinel errors.  It is a pure function: it performs no I/O, opens no connections, and never retries.  Retry policy
// is the caller's responsibility.
//
// Body may be nil or empty; headers may be nil.  Both are inspected tolerantly — any parse failure falls back to the
// most appropriate sentinel rather than returning a raw parse error.
//
// Mapping rules:
//   - 401, 403            → [ErrAuth]
//   - 429                 → *[RateLimitedError] (wraps [ErrRateLimited]) with
//     optional parsed Retry-After
//   - 4xx with context-window body  → [ErrContextLength]
//   - 4xx with model-not-found body → [ErrModelNotFound]
//   - other 4xx           → [ErrUpstream] (generic fallback)
//   - 5xx                 → [ErrUpstream]
//   - anything else       → [ErrUpstream] (defensive fallback)
func ClassifyResponse(statusCode int, headers http.Header, body []byte) error {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrAuth

	case http.StatusTooManyRequests:
		return &RateLimitedError{RetryAfter: parseRetryAfter(headers)}

	default:
		switch {
		case statusCode >= 400 && statusCode < 500:
			return classify4xx(body)
		default:
			// 5xx, network-ish codes (e.g. 0), and any unexpected value all
			// map to ErrUpstream.
			return ErrUpstream
		}
	}
}

// ── Retry-After parsing ───────────────────────────────────────────────────────

// maxRetryAfterSeconds caps an accepted delay-seconds value (24h). Anything larger — or non-finite — is treated as
// unparseable and yields a nil delay, so a garbage header can never produce a negative or absurd backoff.
const maxRetryAfterSeconds = 24 * 60 * 60

// parseRetryAfter extracts and parses the Retry-After header value. It supports both the delay-seconds form and the
// HTTP-date form. Returns nil when the header is absent, unparseable, or out of range.
func parseRetryAfter(headers http.Header) *time.Duration {
	if headers == nil {
		return nil
	}
	val := headers.Get("Retry-After")
	if val == "" {
		return nil
	}

	// Try delay-seconds first (most common in LLM APIs). The upper bound rejects non-finite (Inf/NaN compare false
	// against it) and absurd values, which would otherwise overflow time.Duration to a negative delay.
	if secs, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil && secs >= 0 && secs <= maxRetryAfterSeconds {
		d := time.Duration(secs * float64(time.Second))
		return &d
	}

	// Try HTTP-date (RFC 1123 and variants).
	for _, layout := range []string{
		http.TimeFormat, // "Mon, 02 Jan 2006 15:04:05 GMT"
		time.RFC1123,    // "Mon, 02 Jan 2006 15:04:05 MST"
		time.RFC1123Z,   // "Mon, 02 Jan 2006 15:04:05 -0700"
		"02 Jan 2006 15:04:05 GMT",
	} {
		if t, err := time.Parse(layout, val); err == nil {
			d := max(time.Until(t), 0)
			return &d
		}
	}

	return nil
}

// ── 4xx body classification ───────────────────────────────────────────────────

// classify4xx inspects the response body of a 4xx (non-401/403/429) response and returns the most specific sentinel it
// can identify.  On ambiguity or parse failure it falls back to [ErrUpstream].
func classify4xx(body []byte) error {
	if len(body) == 0 {
		return ErrUpstream
	}

	// Tolerate both top-level {"error": {...}} and {"error": "string"} shapes used by OpenAI-compatible APIs, as well
	// as a flat {"type": "..."} shape.
	type errorDetail struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Param   string `json:"param"`
	}
	var envelope struct {
		Error errorDetail `json:"error"`
		// Some providers hoist type/code to the top level.
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}

	if err := json.Unmarshal(body, &envelope); err != nil {
		// Unparseable body — can't classify more specifically.
		return ErrUpstream
	}

	// Consolidate: prefer the nested error object fields, fall back to top-level.
	errType := cmp.Or(envelope.Error.Type, envelope.Type)
	errCode := cmp.Or(envelope.Error.Code, envelope.Code)
	errMsg := cmp.Or(envelope.Error.Message, envelope.Message)

	if isContextLengthSignal(errType, errCode, errMsg) {
		return ErrContextLength
	}
	if isModelNotFoundSignal(errType, errCode, errMsg) {
		return ErrModelNotFound
	}
	return ErrUpstream
}

// ── Signal matchers ───────────────────────────────────────────────────────────

// isContextLengthSignal reports whether any of the provider error fields indicates a context-window-exceeded condition.
// The matching is deliberately tolerant: a substring/prefix check on lowercased values rather than an exact set, so
// that minor provider variations still classify correctly.
func isContextLengthSignal(errType, errCode, errMsg string) bool {
	lower := strings.ToLower
	combined := lower(errType) + " " + lower(errCode) + " " + lower(errMsg)

	// OpenAI / compatible: code "context_length_exceeded", type "invalid_request_error"
	// Anthropic: type "invalid_request_error", message contains "prompt is too long"
	// Generic: "context_length", "context_window", "maximum context"
	//
	// Deliberately omits bare token-quota phrasing ("token limit", "tokens_limit_reached"): those also appear in
	// billing/quota errors, and misclassifying a quota wall as context-length would make the harness trim the prompt
	// and retry pointlessly. Ambiguous bodies fall back to ErrUpstream.
	keywords := []string{
		"context_length_exceeded",
		"context_length",
		"context_window",
		"prompt is too long",
		"maximum context",
		"input too long",
	}
	for _, kw := range keywords {
		if strings.Contains(combined, kw) {
			return true
		}
	}
	return false
}

// isModelNotFoundSignal reports whether any of the provider error fields indicates that the configured model is not
// available on the upstream.
func isModelNotFoundSignal(errType, errCode, errMsg string) bool {
	lower := strings.ToLower
	combined := lower(errType) + " " + lower(errCode) + " " + lower(errMsg)

	// OpenAI: code "model_not_found", message "The model `…` does not exist"
	// Anthropic: type "not_found_error"
	// Generic catch: "model not found", "no such model"
	//
	// Signals require model adjacency: a bare "does not exist" matches any "resource does not exist" body, and "invalid
	// model" matches "invalid model_parameter" — both generic 4xx that must fall back to ErrUpstream rather than be
	// mistaken for a missing model.
	keywords := []string{
		"model_not_found",
		"model not found",
		"no such model",
		"not_found_error",
		"model does not exist",
		"unknown model",
	}
	for _, kw := range keywords {
		if strings.Contains(combined, kw) {
			return true
		}
	}
	return false
}
