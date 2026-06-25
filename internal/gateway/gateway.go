// Package gateway is the model-gateway layer: it resolves which AI endpoint to use for each capability role, forwards
// requests, and applies resilience policies.  Each exported type in this package is safe for concurrent use unless
// documented otherwise.
//
// The gateway reads its configuration from the "model_gateway" settings namespace on every resolve call (read-through,
// no cached snapshot). Configuration changes written to the settings store take effect on the very next call.
package gateway

import (
	"net/http"

	"github.com/qovira/qovira/internal/store"
)

// Gateway is the entry point to the model-gateway layer.  It resolves capability roles to AI endpoint coordinates and
// forwards requests to the configured upstream.
//
// Construct a Gateway via [New]; the zero value is not valid.
type Gateway struct {
	settings      settingsReader
	httpClient    *http.Client
	resilienceCfg ResilienceConfig
}

// New constructs a Gateway backed by the provided [store.SettingsStore]. The gateway owns the "model_gateway" settings
// namespace — no other component should write to that namespace directly.
//
// The internal HTTP client is intentionally constructed without a wall-clock Timeout: streaming responses are
// legitimately long-lived, and per-request deadlines are managed via the context passed to each call. The resilience
// layer in [chatWithResilience] applies first-token and idle timeout policies on top of the dial seam, using
// [ResilienceConfig] values set here.
func New(ss *store.SettingsStore) *Gateway {
	return &Gateway{
		settings:      ss.Namespace(NamespaceModelGateway),
		httpClient:    newHTTPClient(),
		resilienceCfg: defaultResilienceConfig(),
	}
}
