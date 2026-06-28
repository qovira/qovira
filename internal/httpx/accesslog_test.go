package httpx_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/qovira/qovira/internal/httpx"
)

// captureLogger records a slog.Logger backed by a bytes.Buffer and returns both.
func captureLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return logger, &buf
}

// lastLine returns the last non-empty line from buf.
func lastLine(buf *bytes.Buffer) string {
	lines := bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n"))
	for _, line := range slices.Backward(lines) {
		if len(bytes.TrimSpace(line)) > 0 {
			return string(line)
		}
	}

	return ""
}

// TestAccessLogMiddleware_EmitsOneLinePerRequest verifies that the access-log middleware emits exactly one
// structured slog line per request with method, path, status, duration, and request ID fields.
func TestAccessLogMiddleware_EmitsOneLinePerRequest(t *testing.T) {
	t.Parallel()

	logger, buf := captureLogger(t)

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})

	// Chain: request-ID → access-log → handler.
	h := httpx.NewRequestIDMiddleware(httpx.NewAccessLogMiddleware(logger, inner))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/widgets", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	line := lastLine(buf)
	if line == "" {
		t.Fatal("no log line emitted")
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatalf("log line is not valid JSON: %v\nline: %s", err, line)
	}

	for _, field := range []string{"method", "path", "status", "duration", "requestId"} {
		if _, ok := obj[field]; !ok {
			t.Errorf("log line missing field %q; line: %s", field, line)
		}
	}

	if obj["method"] != "POST" {
		t.Errorf("method: want POST, got %v", obj["method"])
	}

	if obj["path"] != "/api/v1/widgets" {
		t.Errorf("path: want /api/v1/widgets, got %v", obj["path"])
	}

	if obj["status"] != float64(201) {
		t.Errorf("status: want 201, got %v", obj["status"])
	}

	// The request ID in the log must match the Request-Id response header.
	headerID := rr.Header().Get("Request-Id")
	if obj["requestId"] != headerID {
		t.Errorf("log requestId %q differs from response Request-Id %q", obj["requestId"], headerID)
	}
}

// TestAccessLogMiddleware_HealthLogsAtDebug verifies that requests to /api/v1/health emit at debug level,
// not info, so HEALTHCHECK polls don't flood stdout at the default info level.
func TestAccessLogMiddleware_HealthLogsAtDebug(t *testing.T) {
	t.Parallel()

	logger, buf := captureLogger(t)

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	h := httpx.NewRequestIDMiddleware(httpx.NewAccessLogMiddleware(logger, inner))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	_ = rr // rr used only to drive the handler; assertions are on the log line

	line := lastLine(buf)
	if line == "" {
		t.Fatal("no log line emitted")
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatalf("log line is not valid JSON: %v\nline: %s", err, line)
	}

	if obj["level"] != "DEBUG" {
		t.Errorf("health request log level: want DEBUG, got %v", obj["level"])
	}
}

// TestAccessLogMiddleware_NonHealthLogsAtInfo verifies that non-health requests emit at info level.
func TestAccessLogMiddleware_NonHealthLogsAtInfo(t *testing.T) {
	t.Parallel()

	logger, buf := captureLogger(t)

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	h := httpx.NewRequestIDMiddleware(httpx.NewAccessLogMiddleware(logger, inner))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/widgets", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	_ = rr

	line := lastLine(buf)
	if line == "" {
		t.Fatal("no log line emitted")
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatalf("log line is not valid JSON: %v\nline: %s", err, line)
	}

	if obj["level"] != "INFO" {
		t.Errorf("non-health request log level: want INFO, got %v", obj["level"])
	}
}

// TestAccessLogMiddleware_CapturesStatusFromInnerHandler verifies that the captured status in the log
// matches what the inner handler wrote — specifically that the responseWriter wrapper correctly intercepts
// WriteHeader.
func TestAccessLogMiddleware_CapturesStatusFromInnerHandler(t *testing.T) {
	t.Parallel()

	logger, buf := captureLogger(t)

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	h := httpx.NewAccessLogMiddleware(logger, inner)

	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	line := lastLine(buf)
	if line == "" {
		t.Fatal("no log line emitted")
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatalf("log line is not valid JSON: %v\nline: %s", err, line)
	}

	if obj["status"] != float64(404) {
		t.Errorf("captured status: want 404, got %v", obj["status"])
	}
}

// TestAccessLogMiddleware_PanicStatusCaptured verifies that when recovery middleware writes a 500 after a
// panic, the access-log wrapper (which must sit outside recovery) observes the 500 status in its log line.
// This pins the constraint: access-log must be the outermost wrapper so it observes what recovery writes.
func TestAccessLogMiddleware_PanicStatusCaptured(t *testing.T) {
	t.Parallel()

	logger, buf := captureLogger(t)

	panicker := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("test panic")
	})

	// Chain (outermost first): request-ID → access-log → recovery → panicker.
	h := httpx.NewRequestIDMiddleware(
		httpx.NewAccessLogMiddleware(logger,
			httpx.NewRecoveryMiddleware(panicker)))

	req := httptest.NewRequest(http.MethodGet, "/crash", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("panic: want 500, got %d", rr.Code)
	}

	line := lastLine(buf)
	if line == "" {
		t.Fatal("no log line emitted")
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatalf("log line is not valid JSON: %v\nline: %s", err, line)
	}

	if obj["status"] != float64(500) {
		t.Errorf("panic log status: want 500, got %v", obj["status"])
	}

	// requestId must be present in the log line.
	if _, ok := obj["requestId"]; !ok {
		t.Errorf("log line missing requestId after panic; line: %s", line)
	}

	body, _ := io.ReadAll(rr.Body)
	if len(body) == 0 {
		t.Error("500 body is empty — expect a brief generic message")
	}
}

// TestAccessLogMiddleware_PreservesResponseController verifies the status-capturing wrapper is transparent to
// http.ResponseController — a handler below the access log can still Flush (and Hijack, set deadlines, etc.)
// because statusRecorder forwards through Unwrap. Without Unwrap, ResponseController returns "feature not
// supported", silently breaking any future streaming (SSE) handler.
func TestAccessLogMiddleware_PreservesResponseController(t *testing.T) {
	t.Parallel()

	var flushErr error

	h := httpx.NewAccessLogMiddleware(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			flushErr = http.NewResponseController(w).Flush()
		}),
	)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if flushErr != nil {
		t.Errorf("Flush through access-log wrapper: want nil (transparent), got %v", flushErr)
	}
}
