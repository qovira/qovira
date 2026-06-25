package config_test

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qovira/qovira/internal/config"
)

// writeTOML writes content to a temp file and returns the path. The file is cleaned up via t.Cleanup.
func writeTOML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writeTOML: %v", err)
	}
	return path
}

// writeSecret writes a secret value to a temp file and returns the path. The file is cleaned up via t.Cleanup.
func writeSecret(t *testing.T, value string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatalf("writeSecret: %v", err)
	}
	return path
}

// TestLoad_Defaults verifies that when no env or file is set, Load returns sensible default values (no master
// key, so validation will fail — but the defaults for other fields should be observable).
func TestLoad_Defaults(t *testing.T) {
	// Do NOT call t.Parallel() — we use t.Setenv which forbids it.
	t.Setenv("QOVIRA_MASTER_KEY", "")
	t.Setenv("QOVIRA_MASTER_KEY_FILE", "")
	t.Setenv("QOVIRA_DATA_DIR", "")
	t.Setenv("QOVIRA_HTTP_ADDR", "")
	t.Setenv("QOVIRA_LOG_LEVEL", "")
	t.Setenv("QOVIRA_LOG_FORMAT", "")
	t.Setenv("QOVIRA_AUTO_MIGRATE", "")
	t.Setenv("QOVIRA_ADMIN_EMAIL", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD_FILE", "")

	cfg, err := config.Load("")
	// Expect error because master key is missing — but Load still returns a populated *Config on every path, so
	// the default fields below remain observable (a nil cfg here would panic the assertions and fail the test).
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}

	// Defaults should be populated regardless of validation failure.
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("default HTTPAddr = %q, want %q", cfg.HTTPAddr, ":8080")
	}
	if cfg.LogLevel != "info" {
		t.Errorf("default LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
	if cfg.LogFormat != "json" {
		t.Errorf("default LogFormat = %q, want %q", cfg.LogFormat, "json")
	}
	if !cfg.AutoMigrate {
		t.Error("default AutoMigrate should be true")
	}
}

// TestLoad_Precedence_EnvOverFile verifies that env variables override TOML file values (env > file > default
// precedence).
func TestLoad_Precedence_EnvOverFile(t *testing.T) {
	// Set TOML to port 9090, env to port 7070 — env should win.
	tomlPath := writeTOML(t, `
http_addr = ":9090"
log_level = "warn"
`)
	t.Setenv("QOVIRA_HTTP_ADDR", ":7070")
	t.Setenv("QOVIRA_LOG_LEVEL", "")
	t.Setenv("QOVIRA_MASTER_KEY", "this-is-a-long-enough-passphrase-32ch")
	t.Setenv("QOVIRA_MASTER_KEY_FILE", "")
	t.Setenv("QOVIRA_DATA_DIR", "")
	t.Setenv("QOVIRA_LOG_FORMAT", "")
	t.Setenv("QOVIRA_AUTO_MIGRATE", "")
	t.Setenv("QOVIRA_ADMIN_EMAIL", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD_FILE", "")

	cfg, err := config.Load(tomlPath)
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	// Env should win for addr.
	if cfg.HTTPAddr != ":7070" {
		t.Errorf("HTTPAddr = %q, want env value %q", cfg.HTTPAddr, ":7070")
	}
	// File should win for log_level (env was empty).
	if cfg.LogLevel != "warn" {
		t.Errorf("LogLevel = %q, want file value %q", cfg.LogLevel, "warn")
	}
}

// TestLoad_Precedence_FileOverDefault verifies that TOML file values override built-in defaults.
func TestLoad_Precedence_FileOverDefault(t *testing.T) {
	tomlPath := writeTOML(t, `
http_addr = ":9191"
log_format = "json"
`)
	// Clear all env overrides.
	t.Setenv("QOVIRA_MASTER_KEY", "this-is-a-long-enough-passphrase-32ch")
	t.Setenv("QOVIRA_MASTER_KEY_FILE", "")
	t.Setenv("QOVIRA_HTTP_ADDR", "")
	t.Setenv("QOVIRA_LOG_LEVEL", "")
	t.Setenv("QOVIRA_LOG_FORMAT", "")
	t.Setenv("QOVIRA_DATA_DIR", "")
	t.Setenv("QOVIRA_AUTO_MIGRATE", "")
	t.Setenv("QOVIRA_ADMIN_EMAIL", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD_FILE", "")

	cfg, err := config.Load(tomlPath)
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if cfg.HTTPAddr != ":9191" {
		t.Errorf("HTTPAddr = %q, want file value %q", cfg.HTTPAddr, ":9191")
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want file value %q", cfg.LogFormat, "json")
	}
}

// TestLoad_FileSuffix_MasterKey verifies that QOVIRA_MASTER_KEY_FILE reads the secret from the referenced path.
func TestLoad_FileSuffix_MasterKey(t *testing.T) {
	secretPath := writeSecret(t, "my-secret-passphrase-at-least-sixteen-chars\n")

	t.Setenv("QOVIRA_MASTER_KEY", "")
	t.Setenv("QOVIRA_MASTER_KEY_FILE", secretPath)
	t.Setenv("QOVIRA_DATA_DIR", "")
	t.Setenv("QOVIRA_HTTP_ADDR", "")
	t.Setenv("QOVIRA_LOG_LEVEL", "")
	t.Setenv("QOVIRA_LOG_FORMAT", "")
	t.Setenv("QOVIRA_AUTO_MIGRATE", "")
	t.Setenv("QOVIRA_ADMIN_EMAIL", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD_FILE", "")

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	// The secret should be loaded (trailing newline trimmed).
	if string(cfg.MasterKey) != "my-secret-passphrase-at-least-sixteen-chars" {
		t.Errorf("MasterKey = %q (raw), want %q", string(cfg.MasterKey), "my-secret-passphrase-at-least-sixteen-chars")
	}
}

// TestLoad_FileSuffix_AdminPassword verifies that QOVIRA_ADMIN_PASSWORD_FILE reads the secret from the referenced path.
func TestLoad_FileSuffix_AdminPassword(t *testing.T) {
	secretPath := writeSecret(t, "super-secret-admin-password\n")

	t.Setenv("QOVIRA_MASTER_KEY", "this-is-a-long-enough-passphrase-32ch")
	t.Setenv("QOVIRA_MASTER_KEY_FILE", "")
	t.Setenv("QOVIRA_DATA_DIR", "")
	t.Setenv("QOVIRA_HTTP_ADDR", "")
	t.Setenv("QOVIRA_LOG_LEVEL", "")
	t.Setenv("QOVIRA_LOG_FORMAT", "")
	t.Setenv("QOVIRA_AUTO_MIGRATE", "")
	t.Setenv("QOVIRA_ADMIN_EMAIL", "admin@example.com")
	t.Setenv("QOVIRA_ADMIN_PASSWORD", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD_FILE", secretPath)

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if string(cfg.AdminPassword) != "super-secret-admin-password" {
		t.Errorf("AdminPassword = %q (raw), want %q", string(cfg.AdminPassword), "super-secret-admin-password")
	}
}

// TestLoad_FileSuffix_Conflict verifies that setting both QOVIRA_MASTER_KEY
// and QOVIRA_MASTER_KEY_FILE returns an error.
func TestLoad_FileSuffix_Conflict(t *testing.T) {
	secretPath := writeSecret(t, "my-secret-passphrase-at-least-sixteen-chars")

	t.Setenv("QOVIRA_MASTER_KEY", "also-set-directly-and-long-enough-here")
	t.Setenv("QOVIRA_MASTER_KEY_FILE", secretPath)
	t.Setenv("QOVIRA_DATA_DIR", "")
	t.Setenv("QOVIRA_HTTP_ADDR", "")
	t.Setenv("QOVIRA_LOG_LEVEL", "")
	t.Setenv("QOVIRA_LOG_FORMAT", "")
	t.Setenv("QOVIRA_AUTO_MIGRATE", "")
	t.Setenv("QOVIRA_ADMIN_EMAIL", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD_FILE", "")

	_, err := config.Load("")
	if err == nil {
		t.Fatal("expected conflict error when both MASTER_KEY and MASTER_KEY_FILE are set, got nil")
	}
	if !strings.Contains(err.Error(), "MASTER_KEY") {
		t.Errorf("conflict error %q should mention MASTER_KEY", err.Error())
	}
}

// TestLoad_Validation_Aggregated verifies that a Config with multiple bad fields yields ONE error mentioning every
// offending field.
func TestLoad_Validation_Aggregated(t *testing.T) {
	// No master key + bad addr + bad log level + admin email without password.
	t.Setenv("QOVIRA_MASTER_KEY", "")
	t.Setenv("QOVIRA_MASTER_KEY_FILE", "")
	t.Setenv("QOVIRA_HTTP_ADDR", "not-valid-[addr")
	t.Setenv("QOVIRA_LOG_LEVEL", "supercalifragilistic")
	t.Setenv("QOVIRA_LOG_FORMAT", "")
	t.Setenv("QOVIRA_DATA_DIR", "")
	t.Setenv("QOVIRA_AUTO_MIGRATE", "")
	t.Setenv("QOVIRA_ADMIN_EMAIL", "admin@example.com")
	t.Setenv("QOVIRA_ADMIN_PASSWORD", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD_FILE", "")

	_, err := config.Load("")
	if err == nil {
		t.Fatal("expected aggregated validation error, got nil")
	}

	msg := err.Error()
	// Every offending field must be mentioned.
	for _, want := range []string{"master_key", "http_addr", "log_level", "admin_password"} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error missing field %q\ngot: %s", want, msg)
		}
	}
}

// TestLoad_Validation_MasterKeyMinLength verifies that the master key must meet the minimum length requirement (≥ 16
// characters).
func TestLoad_Validation_MasterKeyMinLength(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"exactly 16 chars", "exactly-16chars!", false},
		{"15 chars - too short", "only-15-chars!!", true},
		{"empty", "", true},
		{"well above minimum", "this-is-a-long-enough-passphrase-well-above", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv forbids t.Parallel() in the same test function.
			t.Setenv("QOVIRA_MASTER_KEY", tc.key)
			t.Setenv("QOVIRA_MASTER_KEY_FILE", "")
			t.Setenv("QOVIRA_HTTP_ADDR", "")
			t.Setenv("QOVIRA_LOG_LEVEL", "")
			t.Setenv("QOVIRA_LOG_FORMAT", "")
			t.Setenv("QOVIRA_DATA_DIR", "")
			t.Setenv("QOVIRA_AUTO_MIGRATE", "")
			t.Setenv("QOVIRA_ADMIN_EMAIL", "")
			t.Setenv("QOVIRA_ADMIN_PASSWORD", "")
			t.Setenv("QOVIRA_ADMIN_PASSWORD_FILE", "")

			_, err := config.Load("")
			if tc.wantErr && err == nil {
				t.Errorf("expected error for key %q, got nil", tc.key)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for key %q: %v", tc.key, err)
			}
		})
	}
}

// TestLoad_Validation_MasterKeyNotInError verifies that a short master key error message does NOT contain the actual
// key value.
func TestLoad_Validation_MasterKeyNotInError(t *testing.T) {
	shortKey := "too-short"
	t.Setenv("QOVIRA_MASTER_KEY", shortKey)
	t.Setenv("QOVIRA_MASTER_KEY_FILE", "")
	t.Setenv("QOVIRA_HTTP_ADDR", "")
	t.Setenv("QOVIRA_LOG_LEVEL", "")
	t.Setenv("QOVIRA_LOG_FORMAT", "")
	t.Setenv("QOVIRA_DATA_DIR", "")
	t.Setenv("QOVIRA_AUTO_MIGRATE", "")
	t.Setenv("QOVIRA_ADMIN_EMAIL", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD_FILE", "")

	_, err := config.Load("")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), shortKey) {
		t.Errorf("error message leaks master key value %q: %s", shortKey, err.Error())
	}
}

// TestSecret_Redaction_Stringer verifies that the Secret type's String() method returns "[REDACTED]" and never the
// actual value, covering fmt.Stringer (%v / %s / %+v) and fmt.GoStringer (%#v) paths.
func TestSecret_Redaction_Stringer(t *testing.T) {
	t.Parallel()

	const secretValue = "super-secret-value-do-not-leak"
	s := config.Secret(secretValue)

	// %s is tested via String() directly — staticcheck S1025 correctly notes that fmt.Sprintf("%s", v) is equivalent
	// to v.String(). We verify all other fmt verbs through Sprintf, and %s through its String() method.
	formatters := []struct {
		verb string
		got  string
	}{
		{"%v", fmt.Sprintf("%v", s)},
		{"%s", s.String()},
		{"%+v", fmt.Sprintf("%+v", s)},
		{"%#v", fmt.Sprintf("%#v", s)},
		{"%q", fmt.Sprintf("%q", s)},
	}

	for _, f := range formatters {
		t.Run(f.verb, func(t *testing.T) {
			t.Parallel()
			if strings.Contains(f.got, secretValue) {
				t.Errorf("fmt verb %s leaked secret value: got %q", f.verb, f.got)
			}
			if !strings.Contains(f.got, "[REDACTED]") {
				t.Errorf("fmt verb %s should contain [REDACTED], got %q", f.verb, f.got)
			}
		})
	}
}

// TestSecret_Redaction_Slog verifies that a Config containing secret values does not leak those values when logged
// via slog.
func TestSecret_Redaction_Slog(t *testing.T) {
	t.Parallel()

	const masterKey = "super-secret-master-key-value-32"
	const adminPwd = "super-secret-admin-password-value"

	var buf strings.Builder
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler)

	// Log the secrets directly as slog attributes.
	logger.Info("test",
		"master_key", config.Secret(masterKey),
		"admin_password", config.Secret(adminPwd),
	)

	logged := buf.String()
	if strings.Contains(logged, masterKey) {
		t.Errorf("slog output leaked master key: %s", logged)
	}
	if strings.Contains(logged, adminPwd) {
		t.Errorf("slog output leaked admin password: %s", logged)
	}
	if !strings.Contains(logged, "[REDACTED]") {
		t.Errorf("slog output should contain [REDACTED], got: %s", logged)
	}
}

// TestSecret_Redaction_InConfig verifies that formatting a whole Config struct via %v/%+v does not leak any secret
// values.
func TestSecret_Redaction_InConfig(t *testing.T) {
	t.Setenv("QOVIRA_MASTER_KEY", "this-is-a-long-enough-passphrase-32ch")
	t.Setenv("QOVIRA_MASTER_KEY_FILE", "")
	t.Setenv("QOVIRA_DATA_DIR", "")
	t.Setenv("QOVIRA_HTTP_ADDR", "")
	t.Setenv("QOVIRA_LOG_LEVEL", "")
	t.Setenv("QOVIRA_LOG_FORMAT", "")
	t.Setenv("QOVIRA_AUTO_MIGRATE", "")
	t.Setenv("QOVIRA_ADMIN_EMAIL", "admin@example.com")
	t.Setenv("QOVIRA_ADMIN_PASSWORD", "my-very-secret-admin-pass-here-1234")
	t.Setenv("QOVIRA_ADMIN_PASSWORD_FILE", "")

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}

	for _, verb := range []string{"%v", "%+v", "%s"} {
		got := fmt.Sprintf(verb, cfg)
		if strings.Contains(got, "this-is-a-long-enough-passphrase-32ch") {
			t.Errorf("fmt %s leaked MasterKey value: %s", verb, got)
		}
		if strings.Contains(got, "my-very-secret-admin-pass-here-1234") {
			t.Errorf("fmt %s leaked AdminPassword value: %s", verb, got)
		}
	}
}

// TestLoad_TOMLIgnoresSecrets verifies that a TOML file containing secret-looking keys (master_key, admin_password)
// does not populate those secrets — secrets are env-only.
func TestLoad_TOMLIgnoresSecrets(t *testing.T) {
	tomlPath := writeTOML(t, `
master_key = "from-toml-should-be-ignored"
admin_password = "admin-pass-from-toml-ignored"
http_addr = ":9999"
`)

	t.Setenv("QOVIRA_MASTER_KEY", "this-is-a-long-enough-passphrase-32ch")
	t.Setenv("QOVIRA_MASTER_KEY_FILE", "")
	t.Setenv("QOVIRA_DATA_DIR", "")
	t.Setenv("QOVIRA_HTTP_ADDR", "")
	t.Setenv("QOVIRA_LOG_LEVEL", "")
	t.Setenv("QOVIRA_LOG_FORMAT", "")
	t.Setenv("QOVIRA_AUTO_MIGRATE", "")
	t.Setenv("QOVIRA_ADMIN_EMAIL", "admin@example.com")
	t.Setenv("QOVIRA_ADMIN_PASSWORD", "env-provided-admin-password-long-enough")
	t.Setenv("QOVIRA_ADMIN_PASSWORD_FILE", "")

	cfg, err := config.Load(tomlPath)
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	// Secrets must come from env, not the TOML file.
	if string(cfg.MasterKey) != "this-is-a-long-enough-passphrase-32ch" {
		t.Errorf("MasterKey should be env value, got %q (raw)", string(cfg.MasterKey))
	}
	if string(cfg.AdminPassword) != "env-provided-admin-password-long-enough" {
		t.Errorf("AdminPassword should be env value, got %q (raw)", string(cfg.AdminPassword))
	}
	// Non-secret from TOML should still be loaded (env was empty).
	if cfg.HTTPAddr != ":9999" {
		t.Errorf("HTTPAddr should come from TOML (env empty), got %q", cfg.HTTPAddr)
	}
}

// TestLoad_AdminEmailWithoutPassword verifies that setting admin email without a password produces a validation error.
func TestLoad_AdminEmailWithoutPassword(t *testing.T) {
	t.Setenv("QOVIRA_MASTER_KEY", "this-is-a-long-enough-passphrase-32ch")
	t.Setenv("QOVIRA_MASTER_KEY_FILE", "")
	t.Setenv("QOVIRA_DATA_DIR", "")
	t.Setenv("QOVIRA_HTTP_ADDR", "")
	t.Setenv("QOVIRA_LOG_LEVEL", "")
	t.Setenv("QOVIRA_LOG_FORMAT", "")
	t.Setenv("QOVIRA_AUTO_MIGRATE", "")
	t.Setenv("QOVIRA_ADMIN_EMAIL", "admin@example.com")
	t.Setenv("QOVIRA_ADMIN_PASSWORD", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD_FILE", "")

	_, err := config.Load("")
	if err == nil {
		t.Fatal("expected error for admin email without password, got nil")
	}
	if !strings.Contains(err.Error(), "admin_password") {
		t.Errorf("error should mention admin_password, got: %s", err.Error())
	}
}

// TestLoad_AdminPasswordWithoutEmail verifies that setting admin password without an email produces a validation error.
func TestLoad_AdminPasswordWithoutEmail(t *testing.T) {
	t.Setenv("QOVIRA_MASTER_KEY", "this-is-a-long-enough-passphrase-32ch")
	t.Setenv("QOVIRA_MASTER_KEY_FILE", "")
	t.Setenv("QOVIRA_DATA_DIR", "")
	t.Setenv("QOVIRA_HTTP_ADDR", "")
	t.Setenv("QOVIRA_LOG_LEVEL", "")
	t.Setenv("QOVIRA_LOG_FORMAT", "")
	t.Setenv("QOVIRA_AUTO_MIGRATE", "")
	t.Setenv("QOVIRA_ADMIN_EMAIL", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD", "some-password-here-long-enough")
	t.Setenv("QOVIRA_ADMIN_PASSWORD_FILE", "")

	_, err := config.Load("")
	if err == nil {
		t.Fatal("expected error for admin password without email, got nil")
	}
	if !strings.Contains(err.Error(), "admin_email") {
		t.Errorf("error should mention admin_email, got: %s", err.Error())
	}
}

// TestLoad_Validation_HTTPAddr verifies that the http_addr port is validated beyond structural parsing: out-of-range,
// non-numeric, and empty ports are rejected while well-formed addresses pass.
func TestLoad_Validation_HTTPAddr(t *testing.T) {
	cases := []struct {
		name    string
		addr    string
		wantErr bool
	}{
		{"valid wildcard", ":8080", false},
		{"valid with host", "127.0.0.1:9090", false},
		{"out-of-range port", ":99999", true},
		{"non-numeric port", "host:notaport", true},
		{"empty port", "host:", true},
		// structurally malformed (no colon) is also rejected
		{"no colon", "justhostnoport", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("QOVIRA_MASTER_KEY", "this-is-a-long-enough-passphrase-32ch")
			t.Setenv("QOVIRA_MASTER_KEY_FILE", "")
			t.Setenv("QOVIRA_HTTP_ADDR", tc.addr)
			t.Setenv("QOVIRA_LOG_LEVEL", "")
			t.Setenv("QOVIRA_LOG_FORMAT", "")
			t.Setenv("QOVIRA_DATA_DIR", "")
			t.Setenv("QOVIRA_AUTO_MIGRATE", "")
			t.Setenv("QOVIRA_ADMIN_EMAIL", "")
			t.Setenv("QOVIRA_ADMIN_PASSWORD", "")
			t.Setenv("QOVIRA_ADMIN_PASSWORD_FILE", "")

			_, err := config.Load("")
			if tc.wantErr {
				if err == nil {
					t.Errorf("addr %q: expected http_addr validation error, got nil", tc.addr)
					return
				}
				if !strings.Contains(err.Error(), "http_addr") {
					t.Errorf("addr %q: error should mention http_addr, got: %s", tc.addr, err.Error())
				}
			} else if err != nil {
				t.Errorf("addr %q: unexpected error: %v", tc.addr, err)
			}
		})
	}
}

// TestLoad_Validation_AutoMigrateUnrecognized verifies that an unrecognized QOVIRA_AUTO_MIGRATE value is aggregated as
// a validation error alongside other field errors — i.e. a bad master key AND a bad auto_migrate both appear in the
// single joined error.
func TestLoad_Validation_AutoMigrateUnrecognized(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"true accepted", "true", false},
		{"1 accepted", "1", false},
		{"yes accepted", "yes", false},
		{"false accepted", "false", false},
		{"0 accepted", "0", false},
		{"no accepted", "no", false},
		{"upper-case TRUE accepted", "TRUE", false},
		{"typo tru rejected", "tru", true},
		{"typo fals rejected", "fals", true},
		{"arbitrary word rejected", "enabled", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("QOVIRA_MASTER_KEY", "this-is-a-long-enough-passphrase-32ch")
			t.Setenv("QOVIRA_MASTER_KEY_FILE", "")
			t.Setenv("QOVIRA_HTTP_ADDR", "")
			t.Setenv("QOVIRA_LOG_LEVEL", "")
			t.Setenv("QOVIRA_LOG_FORMAT", "")
			t.Setenv("QOVIRA_DATA_DIR", "")
			t.Setenv("QOVIRA_AUTO_MIGRATE", tc.value)
			t.Setenv("QOVIRA_ADMIN_EMAIL", "")
			t.Setenv("QOVIRA_ADMIN_PASSWORD", "")
			t.Setenv("QOVIRA_ADMIN_PASSWORD_FILE", "")

			_, err := config.Load("")
			if tc.wantErr {
				if err == nil {
					t.Errorf("value %q: expected auto_migrate validation error, got nil", tc.value)
					return
				}
				if !strings.Contains(err.Error(), "auto_migrate") {
					t.Errorf("value %q: error should mention auto_migrate, got: %s", tc.value, err.Error())
				}
			} else if err != nil {
				t.Errorf("value %q: unexpected error: %v", tc.value, err)
			}
		})
	}
}

// TestLoad_Validation_AutoMigrateAggregated verifies that a bad auto_migrate value appears TOGETHER with a master_key
// error in a single aggregated error from errors.Join — both fields must be mentioned in one message.
func TestLoad_Validation_AutoMigrateAggregated(t *testing.T) {
	// No master key (triggers master_key error) + bad auto_migrate value.
	t.Setenv("QOVIRA_MASTER_KEY", "")
	t.Setenv("QOVIRA_MASTER_KEY_FILE", "")
	t.Setenv("QOVIRA_HTTP_ADDR", "")
	t.Setenv("QOVIRA_LOG_LEVEL", "")
	t.Setenv("QOVIRA_LOG_FORMAT", "")
	t.Setenv("QOVIRA_DATA_DIR", "")
	t.Setenv("QOVIRA_AUTO_MIGRATE", "tru")
	t.Setenv("QOVIRA_ADMIN_EMAIL", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD_FILE", "")

	_, err := config.Load("")
	if err == nil {
		t.Fatal("expected aggregated validation error, got nil")
	}

	msg := err.Error()
	if !strings.Contains(msg, "master_key") {
		t.Errorf("aggregated error missing master_key; got: %s", msg)
	}
	if !strings.Contains(msg, "auto_migrate") {
		t.Errorf("aggregated error missing auto_migrate; got: %s", msg)
	}
}

// --- Model gateway seeding configuration (QOVIRA_GATEWAY_*) ---

// TestLoad_GatewayConfig_AllThree verifies that the three QOVIRA_GATEWAY_* variables load into the corresponding Config
// fields and that the API key is redacted across fmt verbs.
func TestLoad_GatewayConfig_AllThree(t *testing.T) {
	t.Setenv("QOVIRA_MASTER_KEY", "a-sufficiently-long-passphrase")
	t.Setenv("QOVIRA_MASTER_KEY_FILE", "")
	t.Setenv("QOVIRA_ADMIN_EMAIL", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD_FILE", "")
	t.Setenv("QOVIRA_GATEWAY_BASE_URL", "https://api.example.com/v1")
	t.Setenv("QOVIRA_GATEWAY_API_KEY", "sk-secret-123")
	t.Setenv("QOVIRA_GATEWAY_API_KEY_FILE", "")
	t.Setenv("QOVIRA_GATEWAY_MODEL", "qwen2.5")

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GatewayBaseURL != "https://api.example.com/v1" {
		t.Errorf("GatewayBaseURL = %q", cfg.GatewayBaseURL)
	}
	if cfg.GatewayModel != "qwen2.5" {
		t.Errorf("GatewayModel = %q", cfg.GatewayModel)
	}
	if string(cfg.GatewayAPIKey) != "sk-secret-123" {
		t.Errorf("GatewayAPIKey raw = %q, want the secret value", string(cfg.GatewayAPIKey))
	}
	// The Secret type must redact the API key across fmt verbs.
	for _, verb := range []string{"%v", "%s", "%#v"} {
		if got := fmt.Sprintf(verb, cfg.GatewayAPIKey); strings.Contains(got, "sk-secret-123") {
			t.Errorf("fmt %s leaked GatewayAPIKey value: %s", verb, got)
		}
	}
}

// TestLoad_FileSuffix_GatewayAPIKey verifies that QOVIRA_GATEWAY_API_KEY_FILE reads the secret from the referenced path
// (with a single trailing newline trimmed).
func TestLoad_FileSuffix_GatewayAPIKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "gateway_api_key")
	if err := os.WriteFile(keyPath, []byte("sk-from-file\n"), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	t.Setenv("QOVIRA_MASTER_KEY", "a-sufficiently-long-passphrase")
	t.Setenv("QOVIRA_MASTER_KEY_FILE", "")
	t.Setenv("QOVIRA_ADMIN_EMAIL", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD", "")
	t.Setenv("QOVIRA_GATEWAY_BASE_URL", "https://api.example.com/v1")
	t.Setenv("QOVIRA_GATEWAY_API_KEY", "")
	t.Setenv("QOVIRA_GATEWAY_API_KEY_FILE", keyPath)
	t.Setenv("QOVIRA_GATEWAY_MODEL", "qwen2.5")

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(cfg.GatewayAPIKey) != "sk-from-file" {
		t.Errorf("GatewayAPIKey = %q (raw), want %q", string(cfg.GatewayAPIKey), "sk-from-file")
	}
}

// TestLoad_GatewayAPIKey_FileConflict verifies that setting both QOVIRA_GATEWAY_API_KEY and its _FILE counterpart is an
// error.
func TestLoad_GatewayAPIKey_FileConflict(t *testing.T) {
	t.Setenv("QOVIRA_MASTER_KEY", "a-sufficiently-long-passphrase")
	t.Setenv("QOVIRA_MASTER_KEY_FILE", "")
	t.Setenv("QOVIRA_GATEWAY_BASE_URL", "https://api.example.com/v1")
	t.Setenv("QOVIRA_GATEWAY_API_KEY", "sk-direct")
	t.Setenv("QOVIRA_GATEWAY_API_KEY_FILE", "/some/path")
	t.Setenv("QOVIRA_GATEWAY_MODEL", "qwen2.5")

	_, err := config.Load("")
	if err == nil {
		t.Fatal("expected conflict error when both GATEWAY_API_KEY and GATEWAY_API_KEY_FILE are set, got nil")
	}
	if !strings.Contains(err.Error(), "GATEWAY_API_KEY") {
		t.Errorf("conflict error %q should mention GATEWAY_API_KEY", err.Error())
	}
}

// TestLoad_Validation_GatewayPartial verifies that setting only some of the QOVIRA_GATEWAY_* variables yields an
// aggregated error naming each missing field — they must be set together or not at all.
func TestLoad_Validation_GatewayPartial(t *testing.T) {
	t.Setenv("QOVIRA_MASTER_KEY", "a-sufficiently-long-passphrase")
	t.Setenv("QOVIRA_MASTER_KEY_FILE", "")
	t.Setenv("QOVIRA_ADMIN_EMAIL", "")
	t.Setenv("QOVIRA_ADMIN_PASSWORD", "")
	t.Setenv("QOVIRA_GATEWAY_BASE_URL", "https://api.example.com/v1")
	t.Setenv("QOVIRA_GATEWAY_API_KEY", "")
	t.Setenv("QOVIRA_GATEWAY_API_KEY_FILE", "")
	t.Setenv("QOVIRA_GATEWAY_MODEL", "")

	_, err := config.Load("")
	if err == nil {
		t.Fatal("expected aggregated validation error for partial gateway config, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"gateway_api_key", "gateway_model"} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error missing field %q\ngot: %s", want, msg)
		}
	}
	// base URL is present, so it must NOT be reported as missing.
	if strings.Contains(msg, "gateway_base_url") {
		t.Errorf("gateway_base_url should not be reported when set; got: %s", msg)
	}
}
