package app_test

import (
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

// ── buildLogger tests ────────────────────────────────────────────────────────

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

	logger.Debug("should be suppressed")
	logger.Info("should be suppressed")
	logger.Warn("should appear")

	out := buf.String()
	if strings.Contains(out, "should be suppressed") {
		t.Errorf("debug/info messages should be suppressed at warn level, got: %q", out)
	}

	if !strings.Contains(out, "should appear") {
		t.Errorf("warn message should appear, got: %q", out)
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

// ── Run tests ────────────────────────────────────────────────────────────────

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

// waitReady polls url until it responds without error or the attempt budget is
// exhausted. Returns the last response (may be nil).
func waitReady(client *http.Client, url string) *http.Response {
	for range 50 {
		resp, err := client.Get(url) //nolint:noctx // test-only polling loop
		if err == nil {
			return resp
		}

		time.Sleep(5 * time.Millisecond)
	}

	return nil
}

func TestRun_HealthzReturns200(t *testing.T) {
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
	url := fmt.Sprintf("http://%s/healthz", addr)

	resp := waitReady(client, url)
	if resp == nil {
		t.Fatalf("server did not become ready at %s", url)
	}

	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Errorf("drain response body: %v", err)
	}

	if err := resp.Body.Close(); err != nil {
		t.Errorf("close response body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz: want 200, got %d", resp.StatusCode)
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
	url := fmt.Sprintf("http://%s/healthz", addr)

	resp := waitReady(client, url)
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
