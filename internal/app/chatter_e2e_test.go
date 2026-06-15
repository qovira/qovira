//go:build e2e

package app

// chatter_e2e_test.go — verifies the Chatter-selector seam under -tags e2e.
//
// In-package test so it can drive the unexported newChatter directly, keeping
// the e2e build's public API free of test-only helpers.
//
// Run with: go test -tags e2e ./internal/app/ -race

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/qovira/qovira/internal/gateway"
)

// TestNewChatterSelector_WithEnvSet verifies that when QOVIRA_E2E_SCRIPT_PATH is
// set to a valid fixture file the selector returns a *ScriptedChatter.
// Cannot use t.Parallel because t.Setenv is not compatible with t.Parallel.
func TestNewChatterSelector_WithEnvSet(t *testing.T) {
	// Write a minimal fixture to a temp file.
	fixturePath := filepath.Join(t.TempDir(), "fixture.json")
	fixtureContent := []byte(`{
		"rules": [
			{
				"match": {"contains": "hello"},
				"rounds": [{"chunks": [{"textDelta": "hi"}, {"done": true}]}]
			}
		]
	}`)
	if err := os.WriteFile(fixturePath, fixtureContent, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	t.Setenv("QOVIRA_E2E_SCRIPT_PATH", fixturePath)

	if _, ok := newChatter(nil).(*gateway.ScriptedChatter); !ok {
		t.Errorf("chatter type = %T, want *gateway.ScriptedChatter", newChatter(nil))
	}
}

// TestNewChatterSelector_WithoutEnv verifies that when QOVIRA_E2E_SCRIPT_PATH is
// not set the selector returns a *gateway.Gateway (the real gateway).
// Cannot use t.Parallel because t.Setenv is not compatible with t.Parallel.
func TestNewChatterSelector_WithoutEnv(t *testing.T) {
	t.Setenv("QOVIRA_E2E_SCRIPT_PATH", "")

	if _, ok := newChatter(nil).(*gateway.Gateway); !ok {
		t.Errorf("chatter type = %T, want *gateway.Gateway", newChatter(nil))
	}
}
