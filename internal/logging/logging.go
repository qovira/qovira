// Package logging constructs the root slog.Logger for the application and establishes the privacy contract for all log
// output.
//
// # Privacy contract
//
// Never log request or message bodies, PII, or secrets. Log identifiers (user ID, request ID) not contents. Secrets
// typed as config.Secret are automatically redacted by slog's LogValuer mechanism. Additionally, any attribute whose
// key matches a known sensitive name (see sensitiveKeys) has its value replaced with "[REDACTED]" before it reaches any
// handler — this catches raw strings accidentally logged under a sensitive key.
//
// # Swappability
//
// NewHandler is the single construction point for concrete handler types. To swap in the OTLP handler (v0.2), replace
// the body of NewHandler with a call to the OTLP constructor — no call sites change.
package logging

import (
	"io"
	"log/slog"
	"strings"

	"github.com/qovira/qovira/internal/config"
)

// sensitiveKeys is the canonical set of attribute key names whose values must never appear in log output. Matching is
// case-insensitive. It is a map (a set, hence the empty-struct values) rather than a slice because replaceAttr looks a
// key up on every single logged attribute: an O(1) membership test on the map beats scanning a slice on the hot path.
// Keep this set in one place; add entries here only — the ReplaceAttr function below is the sole enforcement point.
var sensitiveKeys = map[string]struct{}{
	"password":      {},
	"passwd":        {},
	"secret":        {},
	"token":         {},
	"authorization": {},
	"api_key":       {},
	"apikey":        {},
	"master_key":    {},
	"cookie":        {},
	"set-cookie":    {},
}

// redactedValue is the sentinel string substituted for any sensitive attribute value. It matches the value produced by
// config.Secret.LogValue() so that both paths produce identical output.
const redactedValue = "[REDACTED]"

// isSensitiveKey reports whether key is a member of sensitiveKeys (case-insensitive).
func isSensitiveKey(key string) bool {
	_, ok := sensitiveKeys[strings.ToLower(key)]
	return ok
}

// replaceAttr is the slog.HandlerOptions.ReplaceAttr function installed on every handler built by NewHandler. It
// redacts values whose key is in sensitiveKeys before the handler formats the record.
func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	if isSensitiveKey(a.Key) {
		return slog.String(a.Key, redactedValue)
	}
	return a
}

// NewHandler is the single swap point for handler construction. It builds a slog.Handler that writes to w at the given
// level in the requested format ("json" or "text"). The future OTLP handler is a one-line replacement here.
//
// The returned handler installs replaceAttr to enforce the privacy contract on every attribute, regardless of which
// concrete handler type is in use.
func NewHandler(w io.Writer, level slog.Level, format string) slog.Handler {
	opts := &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: replaceAttr,
	}
	if format == "text" {
		return slog.NewTextHandler(w, opts)
	}
	return slog.NewJSONHandler(w, opts)
}

// ParseLevel converts a validated config log-level string to the corresponding slog.Level. The config package
// guarantees the value is one of debug, info, warn, or error (lowercase), but we map defensively and default to info
// for any unrecognised value.
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// NewLogger constructs a *slog.Logger from the supplied writer and config. It maps cfg.LogLevel to a slog.Level via
// ParseLevel and delegates handler construction to NewHandler so the swap point is honoured.
//
// Pass os.Stdout as w in production (the composition root does this). Pass a *bytes.Buffer or similar in tests to
// capture output without touching the real stdout.
func NewLogger(w io.Writer, cfg config.Config) *slog.Logger {
	level := ParseLevel(cfg.LogLevel)
	h := NewHandler(w, level, cfg.LogFormat)
	return slog.New(h)
}
