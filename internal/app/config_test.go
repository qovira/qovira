package app_test

import (
	"testing"

	"github.com/qovira/qovira/internal/app"
)

// Note: these tests use t.Setenv and therefore cannot use t.Parallel — t.Setenv is incompatible with parallel subtests
// since Go 1.24.

func TestLoadConfig_Defaults(t *testing.T) {
	t.Setenv("QOVIRA_ADDR", "")
	t.Setenv("QOVIRA_LOG_LEVEL", "")
	t.Setenv("QOVIRA_LOG_FORMAT", "")

	cfg, err := app.LoadConfig(app.FlagOverrides{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Addr != ":18888" {
		t.Errorf("Addr: want :18888, got %q", cfg.Addr)
	}

	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel: want info, got %q", cfg.LogLevel)
	}

	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat: want json, got %q", cfg.LogFormat)
	}
}

func TestLoadConfig_EnvOverrides(t *testing.T) {
	t.Setenv("QOVIRA_ADDR", ":9000")
	t.Setenv("QOVIRA_LOG_LEVEL", "debug")
	t.Setenv("QOVIRA_LOG_FORMAT", "text")

	cfg, err := app.LoadConfig(app.FlagOverrides{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Addr != ":9000" {
		t.Errorf("Addr: want :9000, got %q", cfg.Addr)
	}

	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel: want debug, got %q", cfg.LogLevel)
	}

	if cfg.LogFormat != "text" {
		t.Errorf("LogFormat: want text, got %q", cfg.LogFormat)
	}
}

func TestLoadConfig_FlagOverrides(t *testing.T) {
	t.Setenv("QOVIRA_LOG_LEVEL", "warn")
	t.Setenv("QOVIRA_LOG_FORMAT", "json")

	lvl := "error"
	fmt := "text"
	cfg, err := app.LoadConfig(app.FlagOverrides{
		LogLevel:  &lvl,
		LogFormat: &fmt,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// flag wins over env
	if cfg.LogLevel != "error" {
		t.Errorf("LogLevel: want error, got %q", cfg.LogLevel)
	}

	if cfg.LogFormat != "text" {
		t.Errorf("LogFormat: want text, got %q", cfg.LogFormat)
	}
}

func TestLoadConfig_AddrFlagOverridesEnv(t *testing.T) {
	t.Setenv("QOVIRA_ADDR", ":9000")

	addr := ":7777"
	cfg, err := app.LoadConfig(app.FlagOverrides{Addr: &addr})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// flag wins over env
	if cfg.Addr != ":7777" {
		t.Errorf("Addr: want :7777, got %q", cfg.Addr)
	}
}

func TestLoadConfig_InvalidLogLevel(t *testing.T) {
	t.Setenv("QOVIRA_LOG_LEVEL", "trace")
	t.Setenv("QOVIRA_LOG_FORMAT", "")

	_, err := app.LoadConfig(app.FlagOverrides{})
	if err == nil {
		t.Fatal("expected error for invalid log level, got nil")
	}
}

func TestLoadConfig_InvalidLogFormat(t *testing.T) {
	t.Setenv("QOVIRA_LOG_LEVEL", "")
	t.Setenv("QOVIRA_LOG_FORMAT", "yaml")

	_, err := app.LoadConfig(app.FlagOverrides{})
	if err == nil {
		t.Fatal("expected error for invalid log format, got nil")
	}
}

func TestLoadConfig_FlagInvalidLogLevel(t *testing.T) {
	t.Setenv("QOVIRA_LOG_LEVEL", "")
	t.Setenv("QOVIRA_LOG_FORMAT", "")

	lvl := "verbose"
	_, err := app.LoadConfig(app.FlagOverrides{LogLevel: &lvl})
	if err == nil {
		t.Fatal("expected error for invalid log level via flag, got nil")
	}
}
