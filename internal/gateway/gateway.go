// Package gateway is the model-gateway layer: it resolves which AI endpoint to
// use for each capability role, forwards requests, and applies resilience
// policies.  Each exported type in this package is safe for concurrent use
// unless documented otherwise.
//
// The gateway reads its configuration from the "model_gateway" settings
// namespace on every resolve call (read-through, no cached snapshot).
// Configuration changes written to the settings store take effect on the
// very next call.
package gateway

import (
	"github.com/qovira/qovira/internal/store"
)

// Gateway is the entry point to the model-gateway layer.  It resolves
// capability roles to AI endpoint coordinates and (in later slices) forwards
// requests.
//
// Construct a Gateway via [New]; the zero value is not valid.
type Gateway struct {
	settings settingsReader
}

// New constructs a Gateway backed by the provided [store.SettingsStore].
// The gateway owns the "model_gateway" settings namespace — no other component
// should write to that namespace directly.
func New(ss *store.SettingsStore) *Gateway {
	return &Gateway{
		settings: ss.Namespace("model_gateway"),
	}
}
