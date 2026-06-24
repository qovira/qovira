package gateway_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/gateway"
)

// ── AC1: status-code → error mapping ────────────────────────────────────────

func TestClassifyResponse_StatusMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		headers    http.Header
		body       []byte
		wantErr    error // checked via errors.Is
	}{
		// Auth errors
		{
			name:       "401 → ErrAuth",
			statusCode: http.StatusUnauthorized,
			wantErr:    gateway.ErrAuth,
		},
		{
			name:       "403 → ErrAuth",
			statusCode: http.StatusForbidden,
			wantErr:    gateway.ErrAuth,
		},

		// Rate limited (no Retry-After)
		{
			name:       "429 no header → ErrRateLimited",
			statusCode: http.StatusTooManyRequests,
			wantErr:    gateway.ErrRateLimited,
		},

		// 5xx → ErrUpstream
		{
			name:       "500 → ErrUpstream",
			statusCode: http.StatusInternalServerError,
			wantErr:    gateway.ErrUpstream,
		},
		{
			name:       "502 → ErrUpstream",
			statusCode: http.StatusBadGateway,
			wantErr:    gateway.ErrUpstream,
		},
		{
			name:       "503 → ErrUpstream",
			statusCode: http.StatusServiceUnavailable,
			wantErr:    gateway.ErrUpstream,
		},

		// Generic 4xx without a classifiable body → ErrUpstream
		{
			name:       "400 empty body → ErrUpstream",
			statusCode: http.StatusBadRequest,
			body:       nil,
			wantErr:    gateway.ErrUpstream,
		},
		{
			name:       "422 unparseable body → ErrUpstream",
			statusCode: http.StatusUnprocessableEntity,
			body:       []byte("not json"),
			wantErr:    gateway.ErrUpstream,
		},
		{
			name:       "400 unrecognised json body → ErrUpstream",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":{"type":"some_other_error","message":"something went wrong"}}`),
			wantErr:    gateway.ErrUpstream,
		},

		// Context length exceeded
		{
			name:       "400 context_length_exceeded code → ErrContextLength",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"This model's maximum context length is 128000 tokens."}}`),
			wantErr:    gateway.ErrContextLength,
		},
		{
			name:       "400 anthropic prompt too long → ErrContextLength",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"type":"invalid_request_error","message":"prompt is too long: 200000 tokens > 100000 maximum"}`),
			wantErr:    gateway.ErrContextLength,
		},
		{
			name:       "400 context_window keyword → ErrContextLength",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":{"message":"Exceeded the context_window limit for this request."}}`),
			wantErr:    gateway.ErrContextLength,
		},
		{
			name:       "400 input too long → ErrContextLength",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":{"message":"Input too long for this model."}}`),
			wantErr:    gateway.ErrContextLength,
		},

		// Model not found
		{
			name:       "404 model_not_found code → ErrModelNotFound",
			statusCode: http.StatusNotFound,
			body: []byte(`{"error":{"type":"invalid_request_error","code":"model_not_found",` +
				`"message":"The model does not exist or you do not have access to it."}}`),
			wantErr: gateway.ErrModelNotFound,
		},
		{
			name:       "404 anthropic not_found_error → ErrModelNotFound",
			statusCode: http.StatusNotFound,
			body:       []byte(`{"type":"not_found_error","message":"model: no such model"}`),
			wantErr:    gateway.ErrModelNotFound,
		},
		{
			name:       "404 unknown model → ErrModelNotFound",
			statusCode: http.StatusNotFound,
			body:       []byte(`{"error":{"message":"Unknown model: my-fake-model"}}`),
			wantErr:    gateway.ErrModelNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := gateway.ClassifyResponse(tt.statusCode, tt.headers, tt.body)
			if !errors.Is(got, tt.wantErr) {
				t.Errorf("ClassifyResponse(%d, ...) = %v; want errors.Is(_, %v) == true",
					tt.statusCode, got, tt.wantErr)
			}
		})
	}
}

// ── AC2: 429 Retry-After parsing ────────────────────────────────────────────

func TestClassifyResponse_RateLimited_NoRetryAfter(t *testing.T) {
	t.Parallel()

	err := gateway.ClassifyResponse(http.StatusTooManyRequests, nil, nil)

	if !errors.Is(err, gateway.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited; got %v", err)
	}

	var rle *gateway.RateLimitedError
	if !errors.As(err, &rle) {
		t.Fatalf("expected *RateLimitedError; got %T", err)
	}
	if rle.RetryAfter != nil {
		t.Errorf("RetryAfter should be nil when header absent; got %v", rle.RetryAfter)
	}
}

func TestClassifyResponse_RateLimited_RetryAfterSeconds(t *testing.T) {
	t.Parallel()

	headers := http.Header{"Retry-After": []string{"30"}}
	err := gateway.ClassifyResponse(http.StatusTooManyRequests, headers, nil)

	if !errors.Is(err, gateway.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited; got %v", err)
	}

	var rle *gateway.RateLimitedError
	if !errors.As(err, &rle) {
		t.Fatalf("expected *RateLimitedError; got %T", err)
	}
	if rle.RetryAfter == nil {
		t.Fatal("RetryAfter should be non-nil for Retry-After: 30")
	}
	want := 30 * time.Second
	if *rle.RetryAfter != want {
		t.Errorf("RetryAfter = %v; want %v", *rle.RetryAfter, want)
	}
}

func TestClassifyResponse_RateLimited_RetryAfterFractionalSeconds(t *testing.T) {
	t.Parallel()

	headers := http.Header{"Retry-After": []string{"1.5"}}
	err := gateway.ClassifyResponse(http.StatusTooManyRequests, headers, nil)

	var rle *gateway.RateLimitedError
	if !errors.As(err, &rle) {
		t.Fatalf("expected *RateLimitedError; got %T", err)
	}
	if rle.RetryAfter == nil {
		t.Fatal("RetryAfter should be non-nil for Retry-After: 1.5")
	}
	want := 1500 * time.Millisecond
	if *rle.RetryAfter != want {
		t.Errorf("RetryAfter = %v; want %v", *rle.RetryAfter, want)
	}
}

func TestClassifyResponse_RateLimited_RetryAfterHTTPDate(t *testing.T) {
	t.Parallel()

	// Use a date far in the future so time.Until is always > 0 in CI.
	future := time.Now().Add(60 * time.Second).UTC()
	dateStr := future.Format(http.TimeFormat)

	headers := http.Header{"Retry-After": []string{dateStr}}
	err := gateway.ClassifyResponse(http.StatusTooManyRequests, headers, nil)

	var rle *gateway.RateLimitedError
	if !errors.As(err, &rle) {
		t.Fatalf("expected *RateLimitedError; got %T", err)
	}
	if rle.RetryAfter == nil {
		t.Fatal("RetryAfter should be non-nil for HTTP-date Retry-After")
	}
	// Allow a generous window: between 0s and 70s (the parse is a live time.Until call).
	if *rle.RetryAfter < 0 || *rle.RetryAfter > 70*time.Second {
		t.Errorf("RetryAfter = %v; want a value in [0, 70s]", *rle.RetryAfter)
	}
}

// TestClassifyResponse_RateLimited_RetryAfterHTTPDatePast verifies the clamp
// branch: when the HTTP-date in Retry-After is already in the past,
// time.Until(t) is negative and the parser must clamp it to exactly 0 (not a
// negative duration). This is the case the max(time.Until(t), 0) expression
// exists to handle; a future-only test never exercises it.
func TestClassifyResponse_RateLimited_RetryAfterHTTPDatePast(t *testing.T) {
	t.Parallel()

	// Use a date 60 s in the past so time.Until is always negative.
	past := time.Now().Add(-60 * time.Second).UTC()
	dateStr := past.Format(http.TimeFormat)

	headers := http.Header{"Retry-After": []string{dateStr}}
	err := gateway.ClassifyResponse(http.StatusTooManyRequests, headers, nil)

	var rle *gateway.RateLimitedError
	if !errors.As(err, &rle) {
		t.Fatalf("expected *RateLimitedError; got %T", err)
	}
	// A parseable but already-past date must yield a non-nil RetryAfter clamped
	// to exactly zero — not a negative duration.
	if rle.RetryAfter == nil {
		t.Fatal("RetryAfter should be non-nil for a parseable HTTP-date Retry-After (even if past)")
	}
	if *rle.RetryAfter != 0 {
		t.Errorf("RetryAfter = %v; want 0 (clamped from negative time.Until)", *rle.RetryAfter)
	}
}

func TestClassifyResponse_RateLimited_RetryAfterUnparseable(t *testing.T) {
	t.Parallel()

	headers := http.Header{"Retry-After": []string{"not-a-number-or-date"}}
	err := gateway.ClassifyResponse(http.StatusTooManyRequests, headers, nil)

	if !errors.Is(err, gateway.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited; got %v", err)
	}

	var rle *gateway.RateLimitedError
	if !errors.As(err, &rle) {
		t.Fatalf("expected *RateLimitedError; got %T", err)
	}
	if rle.RetryAfter != nil {
		t.Errorf("RetryAfter should be nil for unparseable header; got %v", rle.RetryAfter)
	}
}

// ── AC3: ErrContextLength is distinct from other 4xx ────────────────────────

func TestClassifyResponse_ContextLength_DistinctFrom4xx(t *testing.T) {
	t.Parallel()

	contextBody := []byte(`{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"too many tokens"}}`)
	genericBody := []byte(`{"error":{"type":"invalid_request_error","code":"invalid_api_key","message":"bad key"}}`)

	got := gateway.ClassifyResponse(http.StatusBadRequest, nil, contextBody)
	if !errors.Is(got, gateway.ErrContextLength) {
		t.Errorf("context-window body: want ErrContextLength, got %v", got)
	}
	if errors.Is(got, gateway.ErrAuth) {
		t.Errorf("context-window body must NOT be ErrAuth")
	}
	if errors.Is(got, gateway.ErrUpstream) {
		t.Errorf("context-window body must NOT be ErrUpstream")
	}

	got = gateway.ClassifyResponse(http.StatusBadRequest, nil, genericBody)
	if errors.Is(got, gateway.ErrContextLength) {
		t.Errorf("generic 4xx body must NOT be ErrContextLength; got %v", got)
	}
	if !errors.Is(got, gateway.ErrUpstream) {
		t.Errorf("generic 4xx body: want ErrUpstream, got %v", got)
	}
}

// TestClassifyResponse_FalsePositiveGuards locks in the tolerant "fall back to
// generic on ambiguity" contract: bodies whose wording is adjacent to a specific
// signal but actually denote a different failure must classify as ErrUpstream,
// not be mistaken for context-length or model-not-found.
func TestClassifyResponse_FalsePositiveGuards(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    []byte
		notWant error
	}{
		{
			name:    "token-quota body is not context length",
			body:    []byte(`{"error":{"type":"insufficient_quota","message":"You have reached your token limit for this billing period."}}`),
			notWant: gateway.ErrContextLength,
		},
		{
			name:    "generic resource-does-not-exist is not model not found",
			body:    []byte(`{"error":{"type":"invalid_request_error","message":"The requested resource does not exist."}}`),
			notWant: gateway.ErrModelNotFound,
		},
		{
			name:    "invalid model_parameter is not model not found",
			body:    []byte(`{"error":{"type":"invalid_request_error","message":"Invalid model_parameter: temperature must be <= 2."}}`),
			notWant: gateway.ErrModelNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := gateway.ClassifyResponse(http.StatusBadRequest, nil, tt.body)
			if errors.Is(got, tt.notWant) {
				t.Errorf("body misclassified as %v; want ErrUpstream fallback, got %v", tt.notWant, got)
			}
			if !errors.Is(got, gateway.ErrUpstream) {
				t.Errorf("ambiguous body: want ErrUpstream, got %v", got)
			}
		})
	}
}

// TestClassifyResponse_RateLimited_RetryAfterOutOfRange verifies that non-finite
// and absurdly large Retry-After values yield a nil delay rather than overflowing
// time.Duration into a negative backoff.
func TestClassifyResponse_RateLimited_RetryAfterOutOfRange(t *testing.T) {
	t.Parallel()

	for _, val := range []string{"Inf", "1e12", "-5", "NaN"} {
		t.Run(val, func(t *testing.T) {
			t.Parallel()
			headers := http.Header{"Retry-After": []string{val}}
			err := gateway.ClassifyResponse(http.StatusTooManyRequests, headers, nil)

			var rle *gateway.RateLimitedError
			if !errors.As(err, &rle) {
				t.Fatalf("expected *RateLimitedError; got %T", err)
			}
			if rle.RetryAfter != nil {
				t.Errorf("RetryAfter for %q should be nil; got %v", val, *rle.RetryAfter)
			}
		})
	}
}

// ── AC4: errors.Is works for all sentinels ───────────────────────────────────

func TestSentinels_ErrorsIs(t *testing.T) {
	t.Parallel()

	// Each sentinel must be identifiable via errors.Is from the value returned
	// by ClassifyResponse, not just by direct equality.

	tests := []struct {
		name       string
		statusCode int
		headers    http.Header
		body       []byte
		sentinel   error
	}{
		{
			name:       "ErrAuth via 401",
			statusCode: 401,
			sentinel:   gateway.ErrAuth,
		},
		{
			name:       "ErrRateLimited via 429",
			statusCode: 429,
			sentinel:   gateway.ErrRateLimited,
		},
		{
			name:       "ErrModelNotFound via 404 body",
			statusCode: 404,
			body:       []byte(`{"error":{"code":"model_not_found"}}`),
			sentinel:   gateway.ErrModelNotFound,
		},
		{
			name:       "ErrContextLength via 400 body",
			statusCode: 400,
			body:       []byte(`{"error":{"code":"context_length_exceeded"}}`),
			sentinel:   gateway.ErrContextLength,
		},
		{
			name:       "ErrUpstream via 500",
			statusCode: 500,
			sentinel:   gateway.ErrUpstream,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := gateway.ClassifyResponse(tt.statusCode, tt.headers, tt.body)
			if !errors.Is(got, tt.sentinel) {
				t.Errorf("errors.Is(ClassifyResponse(%d,...), %v) = false; got %v",
					tt.statusCode, tt.sentinel, got)
			}
		})
	}
}

// ── ErrTimeout and ErrUpstreamProtocol are usable with errors.Is ─────────────

func TestSentinels_TimeoutAndProtocolAreComparable(t *testing.T) {
	t.Parallel()

	// These sentinels are returned by the call/parse path, not by ClassifyResponse.
	// Verify they are valid errors and work with errors.Is.
	if !errors.Is(gateway.ErrTimeout, gateway.ErrTimeout) {
		t.Error("errors.Is(ErrTimeout, ErrTimeout) must be true")
	}
	if !errors.Is(gateway.ErrUpstreamProtocol, gateway.ErrUpstreamProtocol) {
		t.Error("errors.Is(ErrUpstreamProtocol, ErrUpstreamProtocol) must be true")
	}
	if errors.Is(gateway.ErrTimeout, gateway.ErrUpstream) {
		t.Error("ErrTimeout must not match ErrUpstream")
	}
	if errors.Is(gateway.ErrUpstreamProtocol, gateway.ErrUpstream) {
		t.Error("ErrUpstreamProtocol must not match ErrUpstream")
	}
}

// ── RateLimitedError.Error() coverage ────────────────────────────────────────

func TestRateLimitedError_ErrorString(t *testing.T) {
	t.Parallel()

	t.Run("without retry-after", func(t *testing.T) {
		t.Parallel()
		rle := &gateway.RateLimitedError{}
		got := rle.Error()
		if got == "" {
			t.Error("Error() must not be empty")
		}
	})

	t.Run("with retry-after", func(t *testing.T) {
		t.Parallel()
		d := 5 * time.Second
		rle := &gateway.RateLimitedError{RetryAfter: &d}
		got := rle.Error()
		if got == "" {
			t.Error("Error() must not be empty")
		}
	})
}

// ── Nil/empty header safety ───────────────────────────────────────────────────

func TestClassifyResponse_NilHeaders(t *testing.T) {
	t.Parallel()

	// Passing nil headers must not panic on any status code.
	codes := []int{401, 403, 429, 404, 400, 500, 502}
	for _, code := range codes {
		t.Run(http.StatusText(code), func(t *testing.T) {
			t.Parallel()
			// Must not panic.
			_ = gateway.ClassifyResponse(code, nil, nil)
		})
	}
}
