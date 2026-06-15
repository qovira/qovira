//go:build e2e

package app

// chatter_e2e.go — Chatter selection for the E2E test build (//go:build e2e).
//
// When QOVIRA_E2E_SCRIPT_PATH is set, newChatter constructs a *gateway.ScriptedChatter
// from the fixture at that path instead of the real *gateway.Gateway.  When the
// env var is absent, the real gateway is used as a fallback (so the e2e binary
// can also run against a live model endpoint when no fixture is supplied).
//
// This file is physically absent from the default binary — the build tag ensures
// it is compiled ONLY when -tags e2e is passed.

import (
	"log/slog"
	"os"

	"github.com/qovira/qovira/internal/gateway"
	"github.com/qovira/qovira/internal/harness"
	"github.com/qovira/qovira/internal/store"
)

// envScriptPath is the environment variable that supplies the fixture file path.
const envScriptPath = "QOVIRA_E2E_SCRIPT_PATH"

// newChatter returns a *gateway.ScriptedChatter when QOVIRA_E2E_SCRIPT_PATH is
// set to a non-empty path, otherwise falls back to the real *gateway.Gateway.
func newChatter(ss *store.SettingsStore) harness.Chatter {
	path := os.Getenv(envScriptPath)
	if path == "" {
		slog.Default().Info("e2e build: QOVIRA_E2E_SCRIPT_PATH not set; using real gateway")
		return gateway.New(ss)
	}
	sc, err := gateway.NewScriptedChatterFromFile(path)
	if err != nil {
		slog.Default().Error("e2e build: failed to load scripted chatter; falling back to real gateway",
			"path", path,
			"err", err,
		)
		return gateway.New(ss)
	}
	slog.Default().Info("e2e build: scripted chatter active", "path", path)
	return sc
}
