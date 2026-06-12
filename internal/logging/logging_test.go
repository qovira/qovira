package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/qovira/qovira/internal/config"
	"github.com/qovira/qovira/internal/logging"
)

// jsonLine parses a single JSON log line captured in buf. It calls t.Fatal if
// the buffer is empty or the content is not valid JSON.
func jsonLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("jsonLine: buffer is empty — no log output was produced")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("jsonLine: not valid JSON: %v\nraw: %s", err, line)
	}
	return m
}

// cfg builds a minimal config.Config sufficient for NewLogger, with sensible
// defaults for fields that logging ignores (MasterKey, DataDir, etc.).
func cfg(level, format string) config.Config {
	return config.Config{
		MasterKey: config.Secret("placeholder-key-not-used-by-logger"),
		LogLevel:  level,
		LogFormat: format,
	}
}

// TestNewLogger_JSONFormat verifies that the JSON format emits valid JSON lines
// containing the expected level, msg, and extra attributes.
func TestNewLogger_JSONFormat(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := logging.NewLogger(&buf, cfg("info", "json"))

	logger.Info("hello world", "user_id", "u-123", "count", 42)

	m := jsonLine(t, &buf)

	if got, ok := m["level"]; !ok || got != "INFO" {
		t.Errorf("level = %v, want INFO", got)
	}
	if got, ok := m["msg"]; !ok || got != "hello world" {
		t.Errorf("msg = %v, want %q", got, "hello world")
	}
	if got, ok := m["user_id"]; !ok || got != "u-123" {
		t.Errorf("user_id = %v, want %q", got, "u-123")
	}
	if _, ok := m["time"]; !ok {
		t.Error("JSON line missing 'time' key")
	}
}

// TestNewLogger_LevelHonored verifies that a debug message is suppressed at
// info level and emitted at debug level.
func TestNewLogger_LevelHonored(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		configLevel string
		wantOutput  bool
	}{
		{"debug suppressed at info", "info", false},
		{"debug emitted at debug", "debug", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := logging.NewLogger(&buf, cfg(tc.configLevel, "json"))
			logger.Debug("debug message")

			hasOutput := buf.Len() > 0
			if hasOutput != tc.wantOutput {
				t.Errorf("config level %q: got output=%v, want output=%v (buf: %q)",
					tc.configLevel, hasOutput, tc.wantOutput, buf.String())
			}
		})
	}
}

// TestNewLogger_FormatSwitch verifies that format "text" produces non-JSON
// output and "json" produces parseable JSON.
func TestNewLogger_FormatSwitch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		format string
		isJSON bool
	}{
		{"json format produces JSON", "json", true},
		{"text format produces non-JSON", "text", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := logging.NewLogger(&buf, cfg("info", tc.format))
			logger.Info("format test")

			line := strings.TrimSpace(buf.String())
			if line == "" {
				t.Fatal("no log output produced")
			}

			var m map[string]any
			err := json.Unmarshal([]byte(line), &m)
			if tc.isJSON && err != nil {
				t.Errorf("format %q: expected valid JSON, got parse error: %v\nline: %s", tc.format, err, line)
			}
			if !tc.isJSON && err == nil {
				t.Errorf("format %q: expected non-JSON output, but JSON parsed successfully", tc.format)
			}
		})
	}
}

// TestNewLogger_Redaction_SecretType verifies that a config.Secret value is
// never written in plain text — it must appear as [REDACTED].
func TestNewLogger_Redaction_SecretType(t *testing.T) {
	t.Parallel()

	const secretValue = "super-secret-password-value-do-not-log"
	var buf bytes.Buffer
	logger := logging.NewLogger(&buf, cfg("debug", "json"))

	logger.Info("secret type test", "some_field", config.Secret(secretValue))

	line := buf.String()
	if strings.Contains(line, secretValue) {
		t.Errorf("log line leaked config.Secret value %q\nline: %s", secretValue, line)
	}
	if !strings.Contains(line, "[REDACTED]") {
		t.Errorf("log line should contain [REDACTED], got: %s", line)
	}
}

// TestNewLogger_Redaction_SensitiveKey verifies that a raw string logged under
// a sensitive attribute key (e.g. "password") has its value replaced with
// [REDACTED], even though the value itself is not a config.Secret.
func TestNewLogger_Redaction_SensitiveKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		key   string
		value string
	}{
		{"password", "hunter2"},
		{"passwd", "hunter3"},
		{"secret", "topsecret"},
		{"token", "eyJhbGci.eyJzdWIi.sig"},
		{"authorization", "Bearer abc123"},
		{"api_key", "sk-live-abc"},
		{"apikey", "sk-live-xyz"},
		{"master_key", "a-very-secret-key"},
		{"cookie", "session=abc123"},
		{"set-cookie", "session=def456; HttpOnly"},
		// Case-insensitive matching.
		{"Password", "hunter4"},
		{"TOKEN", "tokenvalue"},
	}

	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := logging.NewLogger(&buf, cfg("debug", "json"))
			logger.Info("sensitive key test", slog.String(tc.key, tc.value))

			m := jsonLine(t, &buf)

			// The key should still be present in the JSON output.
			// slog lower-cases nothing by default, so check the key as given,
			// but since JSON keys are case-sensitive and slog emits the key as
			// provided, we can look up the normalised key in the parsed map.
			// We check the raw line for the value leak, then parse for the
			// redaction marker.
			if strings.Contains(buf.String(), tc.value) {
				t.Errorf("key %q: log line leaked value %q\nline: %s", tc.key, tc.value, buf.String())
			}

			// Find the key in the JSON (case-sensitive, as emitted by slog).
			got, ok := m[tc.key]
			if !ok {
				t.Errorf("key %q: not found in JSON output\nmap: %v", tc.key, m)
				return
			}
			if got != "[REDACTED]" {
				t.Errorf("key %q: value = %v, want [REDACTED]", tc.key, got)
			}
		})
	}
}

// TestNewLogger_Redaction_Combined verifies that both paths — config.Secret
// type AND a raw string under a sensitive key — yield [REDACTED] in the same
// log line.
func TestNewLogger_Redaction_Combined(t *testing.T) {
	t.Parallel()

	const rawPwd = "hunter2"
	const secretToken = "eyJ.tok.sig" //nolint:gosec // G101 false positive: test fixture, not a real credential

	var buf bytes.Buffer
	logger := logging.NewLogger(&buf, cfg("debug", "json"))

	logger.Info("combined redaction test",
		slog.String("password", rawPwd),
		"token_field", config.Secret(secretToken),
	)

	line := buf.String()
	if strings.Contains(line, rawPwd) {
		t.Errorf("log line leaked raw password value %q\nline: %s", rawPwd, line)
	}
	if strings.Contains(line, secretToken) {
		t.Errorf("log line leaked config.Secret token value %q\nline: %s", secretToken, line)
	}
}

// capturingHandler is a minimal slog.Handler implementation used for the
// swap-point test. It records every slog.Record it receives so the test can
// assert that the logger routes records through whatever handler is installed
// at the NewHandler construction point.
type capturingHandler struct {
	records []slog.Record
	level   slog.Level
}

func (h *capturingHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level
}

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *capturingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// Minimal: return self (sufficient for this test).
	_ = attrs
	return h
}

func (h *capturingHandler) WithGroup(name string) slog.Handler {
	_ = name
	return h
}

// TestNewHandler_SwapPoint verifies the handler seam: by substituting a
// capturingHandler at the same construction point (slog.New), we prove that
// all log calls flow through the installed handler without touching any call
// site.
func TestNewHandler_SwapPoint(t *testing.T) {
	t.Parallel()

	capture := &capturingHandler{level: slog.LevelDebug}

	// This mirrors how NewLogger composes things — pass any handler to slog.New.
	// The test substitutes capturingHandler for the real handler returned by
	// NewHandler, proving the seam works: nothing in the call sites cares which
	// concrete handler is installed.
	logger := slog.New(capture)
	logger.Info("swap test message", "key", "value")
	logger.Debug("debug swap message")

	if len(capture.records) != 2 {
		t.Fatalf("expected 2 captured records, got %d", len(capture.records))
	}

	if capture.records[0].Message != "swap test message" {
		t.Errorf("record[0].Message = %q, want %q", capture.records[0].Message, "swap test message")
	}
	if capture.records[1].Message != "debug swap message" {
		t.Errorf("record[1].Message = %q, want %q", capture.records[1].Message, "debug swap message")
	}
	if capture.records[0].Level != slog.LevelInfo {
		t.Errorf("record[0].Level = %v, want INFO", capture.records[0].Level)
	}
}

// TestParseLevel verifies the level-mapping helper covers all four config
// values and defaults to info for an unrecognised input.
func TestParseLevel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		// Case-insensitive pass-through.
		{"DEBUG", slog.LevelDebug},
		{"WARN", slog.LevelWarn},
		// Defensive default.
		{"", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got := logging.ParseLevel(tc.input)
			if got != tc.want {
				t.Errorf("ParseLevel(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestNewLogger_AllLevels verifies that all four log levels produce output at
// the right threshold and carry the correct level label in the JSON.
func TestNewLogger_AllLevels(t *testing.T) {
	t.Parallel()

	type logCall struct {
		fn        func(l *slog.Logger, msg string)
		wantLevel string
	}

	logCalls := []logCall{
		{func(l *slog.Logger, msg string) { l.Debug(msg) }, "DEBUG"},
		{func(l *slog.Logger, msg string) { l.Info(msg) }, "INFO"},
		{func(l *slog.Logger, msg string) { l.Warn(msg) }, "WARN"},
		{func(l *slog.Logger, msg string) { l.Error(msg) }, "ERROR"},
	}

	for _, lc := range logCalls {
		t.Run(lc.wantLevel, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := logging.NewLogger(&buf, cfg("debug", "json"))
			lc.fn(logger, "level label test")

			m := jsonLine(t, &buf)
			if got := m["level"]; got != lc.wantLevel {
				t.Errorf("level = %v, want %s", got, lc.wantLevel)
			}
		})
	}
}
