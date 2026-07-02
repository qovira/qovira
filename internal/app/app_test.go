package app_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/app"
)

func TestBuildLogger_JSONEmitsJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := app.BuildLogger(testConfig("info", "json"), &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	logger.Info("hello", "key", "value")

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("expected log output, got empty string")
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, line)
	}

	if obj["msg"] != "hello" {
		t.Errorf("msg: want hello, got %v", obj["msg"])
	}

	if obj["key"] != "value" {
		t.Errorf("key: want value, got %v", obj["key"])
	}

	if obj["level"] != "INFO" {
		t.Errorf("level: want INFO, got %v", obj["level"])
	}
}

func TestBuildLogger_TextEmitsText(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := app.BuildLogger(testConfig("info", "text"), &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	logger.Info("hello text")

	out := buf.String()
	if !strings.Contains(out, "hello text") {
		t.Errorf("expected text output to contain message, got: %q", out)
	}

	// text format should NOT parse as JSON
	var obj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &obj); err == nil {
		t.Error("expected non-JSON output for text format, but it parsed as JSON")
	}
}

func TestBuildLogger_RespectsLevel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := app.BuildLogger(testConfig("warn", "json"), &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	logger.Debug("debug msg")
	logger.Info("info msg")
	logger.Warn("warn msg")

	// At warn level only the warn line is emitted; assert on the structured level/msg fields rather than a raw
	// substring so the check can't pass on mangled output.
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want exactly 1 line emitted at warn level, got %d: %q", len(lines), buf.String())
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &obj); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, lines[0])
	}

	if obj["level"] != "WARN" {
		t.Errorf("level: want WARN, got %v", obj["level"])
	}

	if obj["msg"] != "warn msg" {
		t.Errorf("msg: want 'warn msg', got %v", obj["msg"])
	}
}

func TestBuildLogger_InvalidFormat(t *testing.T) {
	t.Parallel()

	_, err := app.BuildLogger(testConfig("info", "yaml"), io.Discard)
	if err == nil {
		t.Fatal("expected error for invalid log format, got nil")
	}
}

func TestBuildLogger_InvalidLevel(t *testing.T) {
	t.Parallel()

	_, err := app.BuildLogger(testConfig("verbose", "json"), io.Discard)
	if err == nil {
		t.Fatal("expected error for invalid log level, got nil")
	}
}

// testConfig constructs a minimal app.Config for use in tests.
func testConfig(level, format string) app.Config {
	return app.Config{
		Addr:      ":0",
		LogLevel:  level,
		LogFormat: format,
	}
}

// freePort asks the OS for an available TCP port then releases it.
func freePort(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}

	addr := l.Addr().String()

	if err := l.Close(); err != nil {
		t.Fatalf("close probe listener: %v", err)
	}

	return addr
}

// waitReady polls url until it responds with a non-5xx status or the attempt budget is exhausted.
// A >= 500 response is treated as not-yet-ready (transient startup error) and the poll continues.
// Returns the last response that was deemed ready (may be nil if the server never became ready).
// Bodies of discarded not-ready responses are drained and closed to avoid leaking connections.
func waitReady(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()

	for range 50 {
		resp, err := client.Get(url) //nolint:noctx // test-only polling loop
		if err != nil {
			time.Sleep(5 * time.Millisecond)
			continue
		}

		if resp.StatusCode >= 500 {
			// Transient server-side error during startup — drain and discard, keep polling.
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()

			time.Sleep(5 * time.Millisecond)

			continue
		}

		return resp
	}

	return nil
}

func TestRun_HealthReturns200WithJSON(t *testing.T) {
	t.Parallel()

	addr := freePort(t)
	cfg := app.Config{Addr: addr, LogLevel: "error", LogFormat: "json"}

	// Explicit cancel so we can trigger shutdown inside the test body.
	//nolint:testingcontext // need to cancel manually before test returns
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)

	go func() {
		runErr <- app.Run(ctx, cfg)
	}()

	client := &http.Client{Timeout: time.Second}
	healthURL := fmt.Sprintf("http://%s/api/v1/health", addr)

	// Wait for the server to be up — waitReady returns on the first successful HTTP response (any status).
	// We then re-read the response properly below.
	resp := waitReady(t, client, healthURL)
	if resp == nil {
		t.Fatalf("server did not become ready at %s", healthURL)
	}

	// Drain and close the poll response.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// Issue a fresh request to read the JSON body accurately.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	resp2, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/health: %v", err)
	}

	defer func() {
		_, _ = io.Copy(io.Discard, resp2.Body)
		_ = resp2.Body.Close()
	}()

	if resp2.StatusCode != http.StatusOK {
		t.Errorf("GET /api/v1/health: want 200, got %d", resp2.StatusCode)
	}

	// Decode and assert the JSON payload carries status:"ok".
	var payload struct {
		Status string `json:"status"`
	}

	if err := json.NewDecoder(resp2.Body).Decode(&payload); err != nil {
		t.Fatalf("decode JSON body: %v", err)
	}

	if payload.Status != "ok" {
		t.Errorf("health status: want %q, got %q", "ok", payload.Status)
	}

	// Cancel the context and assert Run returns nil within a tight deadline.
	cancel()

	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run returned non-nil error after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Run did not return within 5 s after context cancel")
	}
}

func TestRun_InvalidAddrReturnsError(t *testing.T) {
	t.Parallel()

	// An address that is syntactically invalid / cannot be bound.
	cfg := app.Config{Addr: "%%invalid%%", LogLevel: "error", LogFormat: "json"}

	err := app.Run(t.Context(), cfg)
	if err == nil {
		t.Fatal("expected error for invalid addr, got nil")
	}
}

func TestRun_AlreadyBoundAddrReturnsError(t *testing.T) {
	t.Parallel()

	// Hold the port open so Run cannot bind it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("setup listener: %v", err)
	}

	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Errorf("close hold listener: %v", err)
		}
	})

	cfg := app.Config{Addr: l.Addr().String(), LogLevel: "error", LogFormat: "json"}

	if err := app.Run(t.Context(), cfg); err == nil {
		t.Fatal("expected error for already-bound addr, got nil")
	}
}

// TestRun_SLOGIsSetAsDefault verifies that Run calls slog.SetDefault.
// Not parallel — touches the process-global slog default logger.
func TestRun_SLOGIsSetAsDefault(t *testing.T) {
	addr := freePort(t)
	cfg := app.Config{Addr: addr, LogLevel: "error", LogFormat: "json"}

	// Restore the previous default logger when the test ends.
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	//nolint:testingcontext // need to cancel manually before test returns
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)

	go func() {
		runErr <- app.Run(ctx, cfg)
	}()

	// Poll until the server is up so we know Run has executed slog.SetDefault.
	client := &http.Client{Timeout: time.Second}
	url := fmt.Sprintf("http://%s/api/v1/health", addr)

	resp := waitReady(t, client, url)
	if resp != nil {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}

	// The default logger should differ from the stdlib default.
	if slog.Default() == prev {
		t.Error("slog.Default() was not changed by Run")
	}

	cancel()

	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run: want nil, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Run did not return within 5 s")
	}
}

// TestRun_PostEventsDoesNotStartSSEStream asserts a non-GET request never starts an SSE stream. POST /events
// returns 200 from the SPA catch-all, not 405: Go's automatic 405 fires only when a method-less pattern like
// "/events" is also registered, but here only "GET /events" and "/" exist, so the POST falls through to the
// SPA handler. Either way, no SSE headers or streaming body are produced.
func TestRun_PostEventsDoesNotStartSSEStream(t *testing.T) {
	t.Parallel()

	addr := freePort(t)
	cfg := app.Config{Addr: addr, LogLevel: "error", LogFormat: "json"}

	//nolint:testingcontext // need to cancel manually before test returns
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)

	go func() {
		runErr <- app.Run(ctx, cfg)
	}()

	client := &http.Client{Timeout: 3 * time.Second}
	healthURL := fmt.Sprintf("http://%s/api/v1/health", addr)

	if resp := waitReady(t, client, healthURL); resp != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	eventsURL := fmt.Sprintf("http://%s/events", addr)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, eventsURL, nil)
	if err != nil {
		t.Fatalf("build POST /events request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /events: %v", err)
	}

	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	// The SSE handler must NOT have run: assert that Content-Type is NOT text/event-stream.
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		t.Errorf("POST /events: SSE handler must not run for non-GET — got Content-Type: %s", ct)
	}

	// Read the body with a deadline to detect any accidental streaming. A legitimate SPA or error
	// response terminates immediately; an SSE stream would hang here.

	// readResult carries scanner.Err() back so it's checked on the test goroutine, not the reader, whose
	// t.Errorf would race the test's return on the timeout path below.
	type readResult struct {
		body string
		err  error
	}

	bodyCh := make(chan readResult, 1)

	go func() {
		var sb strings.Builder
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			sb.WriteString(scanner.Text())
			sb.WriteByte('\n')
		}

		bodyCh <- readResult{body: sb.String(), err: scanner.Err()}
	}()

	select {
	case res := <-bodyCh:
		if res.err != nil {
			t.Errorf("POST /events: read response body: %v", res.err)
		}

		// Body read to completion — not an SSE stream.
		t.Logf("POST /events → status=%d Content-Type=%q body-len=%d (SPA fallthrough confirmed, not SSE)",
			resp.StatusCode, ct, len(res.body))
	case <-time.After(2 * time.Second):
		t.Error("POST /events: response body did not terminate — possible accidental SSE streaming")
	}

	cancel()

	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run returned non-nil error after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Run did not return within 5 s after context cancel")
	}
}
