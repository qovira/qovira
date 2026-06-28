// Package app is the composition root for the Qovira application server. It reads env config, builds the slog
// logger, wires the HTTP server, and owns the run / graceful-shutdown lifecycle.
package app

import (
	"fmt"
	"os"
	"slices"
)

// Config holds the resolved application configuration. Fields are populated by [LoadConfig] from environment
// variables and optional flag overrides.
type Config struct {
	// Addr is the TCP address for the HTTP server to listen on (e.g. ":18888").
	Addr string

	// LogLevel is one of: debug, info, warn, error.
	LogLevel string

	// LogFormat is one of: json, text.
	LogFormat string
}

// FlagOverrides carries CLI flag values that win over env vars when non-nil. A nil pointer means "not set by
// the user" (env or default applies).
type FlagOverrides struct {
	LogLevel  *string
	LogFormat *string
}

var (
	validLogLevels  = []string{"debug", "info", "warn", "error"}
	validLogFormats = []string{"json", "text"}
)

// LoadConfig resolves the application configuration by reading environment variables and applying flag
// overrides. The precedence order (highest wins) is: flag → env → built-in default.
func LoadConfig(flags FlagOverrides) (Config, error) {
	cfg := Config{
		Addr:      getEnvOr("QOVIRA_ADDR", ":18888"),
		LogLevel:  getEnvOr("QOVIRA_LOG_LEVEL", "info"),
		LogFormat: getEnvOr("QOVIRA_LOG_FORMAT", "json"),
	}

	// Flag overrides win when explicitly set by the user.
	if flags.LogLevel != nil {
		cfg.LogLevel = *flags.LogLevel
	}

	if flags.LogFormat != nil {
		cfg.LogFormat = *flags.LogFormat
	}

	// Validate.
	if !slices.Contains(validLogLevels, cfg.LogLevel) {
		return Config{}, fmt.Errorf("invalid QOVIRA_LOG_LEVEL %q: must be one of %v", cfg.LogLevel, validLogLevels)
	}

	if !slices.Contains(validLogFormats, cfg.LogFormat) {
		return Config{}, fmt.Errorf("invalid QOVIRA_LOG_FORMAT %q: must be one of %v", cfg.LogFormat, validLogFormats)
	}

	return cfg, nil
}

// getEnvOr returns the value of the environment variable named by key, or defaultVal if the variable is unset
// or empty. An empty-string env value is treated as unset: this is intentional so operators can blank a
// variable to restore the built-in default without removing it from their environment file.
func getEnvOr(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return defaultVal
}
