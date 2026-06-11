// Package config implements an explicit env-first configuration loader for
// Qovira's boot configuration — the settings needed before the encrypted
// database opens. It cannot live in the DB; this package owns it.
//
// Precedence: env > optional TOML file > built-in defaults.
//
// Secrets (MasterKey, AdminPassword) are env-only — they are never read from
// the TOML file. Both support _FILE indirection: set QOVIRA_MASTER_KEY_FILE
// (or QOVIRA_ADMIN_PASSWORD_FILE) to a path whose contents are used as the
// secret. Setting both the direct env var and its _FILE counterpart is an
// error.
//
// Master-key minimum length: 16 bytes.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// masterKeyMinLen is the minimum acceptable length for the master key
// passphrase. 16 bytes is the floor; operators are encouraged to use
// longer passphrases.
const masterKeyMinLen = 16

// Secret is a string type that structurally prevents its value from leaking
// via fmt verbs or slog. All fmt.Stringer, fmt.GoStringer, and slog.LogValuer
// methods return a redacted placeholder, so the actual value never appears in
// log lines or error messages regardless of calling code discipline.
type Secret string

// String implements fmt.Stringer. Returns "[REDACTED]" so %v/%s never leaks
// the value.
func (Secret) String() string { return "[REDACTED]" }

// GoString implements fmt.GoStringer. Returns `config.Secret("[REDACTED]")` so
// %#v never leaks the value.
func (Secret) GoString() string { return `config.Secret("[REDACTED]")` }

// LogValue implements slog.LogValuer. Returns a redacted slog.Value so any
// slog handler never records the actual value.
func (Secret) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// tomlFile is the thin TOML surface that the config file may carry.
// Secrets (master_key, admin_password) are deliberately absent — they are
// env-only. If a caller puts secret-looking keys in the file they are silently
// ignored (they simply don't map to any field here).
type tomlFile struct {
	DataDir     string `toml:"data_dir"`
	HTTPAddr    string `toml:"http_addr"`
	LogLevel    string `toml:"log_level"`
	LogFormat   string `toml:"log_format"`
	AutoMigrate *bool  `toml:"auto_migrate"`
}

// Config holds all boot-time configuration resolved from env, file, and
// defaults. Fields are exported so the serve command (and tests) can inspect
// them; secrets are typed as Secret to prevent accidental logging.
type Config struct {
	// MasterKey is the passphrase used to derive the encryption key for the
	// database. Required; minimum 16 bytes; env-only (never from TOML).
	MasterKey Secret

	// DataDir is the directory where application data (including the DB) is
	// stored. Default: "./data".
	DataDir string

	// HTTPAddr is the TCP address on which the HTTP server listens.
	// Default: ":8080".
	HTTPAddr string

	// LogLevel controls the slog minimum level. Accepted: debug, info, warn,
	// error. Default: "info".
	LogLevel string

	// LogFormat selects the slog output format. Accepted: text, json.
	// Default: "json".
	LogFormat string

	// AutoMigrate controls whether the server runs database migrations on
	// startup. Default: true.
	AutoMigrate bool

	// AdminEmail, if set together with AdminPassword, seeds an initial admin
	// account on first run. Both must be set together or not at all.
	AdminEmail string

	// AdminPassword is the initial admin account password. Env-only.
	AdminPassword Secret

	// rawAutoMigrate captures the literal QOVIRA_AUTO_MIGRATE env value so
	// validate can produce an aggregated error for unrecognized values.
	rawAutoMigrate string
}

// Load resolves boot configuration from the environment, an optional TOML
// file, and built-in defaults (in that order of precedence).
//
// cfgPath is the path to the TOML file. Pass an empty string to skip file
// loading (the server still boots from env + defaults).
//
// Load always returns the partially-resolved *Config alongside any error, so
// callers can inspect defaults even when validation fails. The returned error
// is an aggregated list of every field that is missing or invalid.
func Load(cfgPath string) (*Config, error) {
	// Step 1: apply defaults.
	cfg := &Config{
		DataDir:     "./data",
		HTTPAddr:    ":8080",
		LogLevel:    "info",
		LogFormat:   "json",
		AutoMigrate: true,
	}

	// Step 2: overlay TOML file values (non-secret only).
	if cfgPath != "" {
		if err := loadTOML(cfg, cfgPath); err != nil {
			return cfg, fmt.Errorf("config file: %w", err)
		}
	}

	// Step 3: overlay environment variables (including secrets).
	if err := loadEnv(cfg); err != nil {
		// loadEnv returns hard errors (e.g. _FILE conflict, unreadable file).
		return cfg, err
	}

	// Step 4: validate every field and aggregate all problems.
	if err := validate(cfg); err != nil {
		return cfg, err
	}

	return cfg, nil
}

// loadTOML reads cfgPath and overlays non-secret operator preferences onto cfg.
func loadTOML(cfg *Config, cfgPath string) error {
	var f tomlFile
	if _, err := toml.DecodeFile(cfgPath, &f); err != nil {
		return err
	}
	if f.DataDir != "" {
		cfg.DataDir = f.DataDir
	}
	if f.HTTPAddr != "" {
		cfg.HTTPAddr = f.HTTPAddr
	}
	if f.LogLevel != "" {
		cfg.LogLevel = f.LogLevel
	}
	if f.LogFormat != "" {
		cfg.LogFormat = f.LogFormat
	}
	if f.AutoMigrate != nil {
		cfg.AutoMigrate = *f.AutoMigrate
	}
	return nil
}

// loadEnv reads QOVIRA_* environment variables and overlays them onto cfg.
// Returns a hard error only for _FILE conflicts or unreadable secret files.
func loadEnv(cfg *Config) error {
	// --- Secrets via _FILE indirection ---
	// MasterKey: prefer _FILE when it's the sole source; error on conflict.
	masterKeyDirect := os.Getenv("QOVIRA_MASTER_KEY")
	masterKeyFile := os.Getenv("QOVIRA_MASTER_KEY_FILE")
	switch {
	case masterKeyDirect != "" && masterKeyFile != "":
		return errors.New("QOVIRA_MASTER_KEY and QOVIRA_MASTER_KEY_FILE are both set; use exactly one")
	case masterKeyFile != "":
		val, err := readSecretFile(masterKeyFile)
		if err != nil {
			return fmt.Errorf("QOVIRA_MASTER_KEY_FILE: %w", err)
		}
		cfg.MasterKey = Secret(val)
	case masterKeyDirect != "":
		cfg.MasterKey = Secret(masterKeyDirect)
	}

	// AdminPassword: same _FILE indirection logic.
	adminPwdDirect := os.Getenv("QOVIRA_ADMIN_PASSWORD")
	adminPwdFile := os.Getenv("QOVIRA_ADMIN_PASSWORD_FILE")
	switch {
	case adminPwdDirect != "" && adminPwdFile != "":
		return errors.New("QOVIRA_ADMIN_PASSWORD and QOVIRA_ADMIN_PASSWORD_FILE are both set; use exactly one")
	case adminPwdFile != "":
		val, err := readSecretFile(adminPwdFile)
		if err != nil {
			return fmt.Errorf("QOVIRA_ADMIN_PASSWORD_FILE: %w", err)
		}
		cfg.AdminPassword = Secret(val)
	case adminPwdDirect != "":
		cfg.AdminPassword = Secret(adminPwdDirect)
	}

	// --- Non-secret env overrides ---
	if v := os.Getenv("QOVIRA_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("QOVIRA_HTTP_ADDR"); v != "" {
		cfg.HTTPAddr = v
	}
	if v := os.Getenv("QOVIRA_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("QOVIRA_LOG_FORMAT"); v != "" {
		cfg.LogFormat = v
	}
	if v := os.Getenv("QOVIRA_AUTO_MIGRATE"); v != "" {
		cfg.rawAutoMigrate = v
		switch strings.ToLower(v) {
		case "true", "1", "yes":
			cfg.AutoMigrate = true
		case "false", "0", "no":
			cfg.AutoMigrate = false
		default:
			// Unrecognized values are recorded in rawAutoMigrate; validate will
			// produce an aggregated error for them.
		}
	}
	if v := os.Getenv("QOVIRA_ADMIN_EMAIL"); v != "" {
		cfg.AdminEmail = v
	}

	return nil
}

// readSecretFile reads the content of path, trims a single trailing newline
// (common when secrets are written by shell echo or Docker secrets), and
// returns the result. The file is opened read-only.
//
// The path comes from a trusted operator-controlled environment variable
// (_FILE indirection). Path traversal is an accepted, documented operator
// responsibility; the process must have access to the secret file by design.
func readSecretFile(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-provided _FILE path; intentional
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\n\r"), nil
}

// validate runs one pass over cfg and returns an aggregated error listing
// every field that is missing or invalid. Returns nil if everything is valid.
func validate(cfg *Config) error {
	var errs []error

	// Master key: required, minimum length.
	switch {
	case len(cfg.MasterKey) == 0:
		errs = append(errs, errors.New("master_key: required (set QOVIRA_MASTER_KEY or QOVIRA_MASTER_KEY_FILE)"))
	case len(cfg.MasterKey) < masterKeyMinLen:
		// Do NOT include the key value in the message.
		errs = append(errs, fmt.Errorf("master_key: must be at least %d bytes", masterKeyMinLen))
	}

	// HTTP listen address: must be host:port with a valid, non-empty port number
	// in the range 1..65535. An empty host (":8080") is fine; an empty or
	// non-numeric port is not.
	if cfg.HTTPAddr != "" {
		_, port, splitErr := net.SplitHostPort(cfg.HTTPAddr)
		switch {
		case splitErr != nil:
			errs = append(errs, fmt.Errorf("http_addr: invalid address %q: %w", cfg.HTTPAddr, splitErr))
		case port == "":
			errs = append(errs, fmt.Errorf("http_addr: invalid address %q: port is required", cfg.HTTPAddr))
		default:
			n, convErr := strconv.Atoi(port)
			if convErr != nil {
				errs = append(errs, fmt.Errorf("http_addr: invalid address %q: port %q is not numeric", cfg.HTTPAddr, port))
			} else if n < 1 || n > 65535 {
				errs = append(errs, fmt.Errorf("http_addr: invalid address %q: port %d is out of range 1–65535", cfg.HTTPAddr, n))
			}
		}
	}

	// AutoMigrate: reject unrecognized env values rather than silently ignoring.
	if cfg.rawAutoMigrate != "" {
		switch strings.ToLower(cfg.rawAutoMigrate) {
		case "true", "1", "yes", "false", "0", "no":
			// already applied in loadEnv; nothing to do here.
		default:
			errs = append(errs, fmt.Errorf("auto_migrate: unrecognized value %q (accepted: true, 1, yes, false, 0, no)", cfg.rawAutoMigrate))
		}
	}

	// Log level: only the four slog levels are accepted.
	switch strings.ToLower(cfg.LogLevel) {
	case "debug", "info", "warn", "error":
		cfg.LogLevel = strings.ToLower(cfg.LogLevel)
	default:
		errs = append(errs, fmt.Errorf("log_level: invalid value %q (accepted: debug, info, warn, error)", cfg.LogLevel))
	}

	// Log format: text or json only.
	switch strings.ToLower(cfg.LogFormat) {
	case "text", "json":
		cfg.LogFormat = strings.ToLower(cfg.LogFormat)
	default:
		errs = append(errs, fmt.Errorf("log_format: invalid value %q (accepted: text, json)", cfg.LogFormat))
	}

	// Admin seed: email and password must be set together or not at all.
	hasEmail := cfg.AdminEmail != ""
	hasPassword := len(cfg.AdminPassword) > 0
	if hasEmail && !hasPassword {
		errs = append(errs, errors.New("admin_password: required when QOVIRA_ADMIN_EMAIL is set"))
	}
	if hasPassword && !hasEmail {
		errs = append(errs, errors.New("admin_email: required when QOVIRA_ADMIN_PASSWORD is set"))
	}

	return errors.Join(errs...)
}
