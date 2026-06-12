package gateway

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

// ErrGatewayNotConfigured is returned by [Gateway.resolve] when no primary
// endpoint has been configured.  Callers should surface this as a
// user-actionable configuration error rather than an infrastructure failure.
var ErrGatewayNotConfigured = errors.New("gateway: no primary endpoint configured")

// Role identifies a model capability.  Only [RoleChat] is active in v0.1;
// the remaining roles are reserved for future slices.
type Role string

const (
	// RoleChat is the default role used for conversational turns.
	RoleChat Role = "chat"
	// RoleEmbeddings is reserved for text-embedding requests.
	RoleEmbeddings Role = "embeddings"
	// RoleVision is reserved for multimodal (image-understanding) requests.
	RoleVision Role = "vision"
	// RoleSTT is reserved for speech-to-text transcription requests.
	RoleSTT Role = "stt"
	// RoleTTS is reserved for text-to-speech synthesis requests.
	RoleTTS Role = "tts"
)

// Resolved holds the fully-resolved endpoint coordinates for a single role
// after primary settings and per-role overrides have been merged.
type Resolved struct {
	BaseURL string
	APIKey  string
	Model   string
}

// settingsReader is a consumer-side interface satisfied by
// [*store.SettingsNamespace].  The narrow interface lets tests inject a pure
// in-memory fake without requiring CGO or a live SQLCipher database.
type settingsReader interface {
	Get(ctx context.Context, key string) (string, bool, error)
}

// resolve merges the primary endpoint configuration with any per-role
// overrides and returns the resulting [Resolved] coordinates.
//
// Resolution rules:
//   - Primary fields are read from keys "primary.baseURL", "primary.apiKey",
//     and "primary.model".
//   - If any primary field is absent or empty the primary is considered
//     unconfigured and [ErrGatewayNotConfigured] is returned.
//   - Role-specific overrides are read from "roles.<role>.baseURL",
//     "roles.<role>.apiKey", and "roles.<role>.model".  Any override field
//     that is present and non-empty replaces the corresponding primary value;
//     absent or empty override fields fall through to primary.
//
// resolve reads through to the store on every call — there is no cached
// snapshot and no invalidation required.
func (g *Gateway) resolve(ctx context.Context, role Role) (Resolved, error) {
	primary, err := g.readPrimary(ctx)
	if err != nil {
		return Resolved{}, err
	}

	override, err := g.readRole(ctx, role)
	if err != nil {
		return Resolved{}, err
	}

	merged := primary
	if override.BaseURL != "" {
		merged.BaseURL = override.BaseURL
	}
	if override.APIKey != "" {
		merged.APIKey = override.APIKey
	}
	if override.Model != "" {
		merged.Model = override.Model
	}

	return merged, nil
}

// readPrimary reads the three primary-endpoint keys and returns them.  If
// any of the three required fields (baseURL, apiKey, model) is absent or
// empty, [ErrGatewayNotConfigured] is returned.
func (g *Gateway) readPrimary(ctx context.Context) (Resolved, error) {
	baseURL, err := g.getString(ctx, "primary.baseURL")
	if err != nil {
		return Resolved{}, err
	}
	apiKey, err := g.getString(ctx, "primary.apiKey")
	if err != nil {
		return Resolved{}, err
	}
	model, err := g.getString(ctx, "primary.model")
	if err != nil {
		return Resolved{}, err
	}

	if baseURL == "" || apiKey == "" || model == "" {
		return Resolved{}, ErrGatewayNotConfigured
	}

	return Resolved{BaseURL: baseURL, APIKey: apiKey, Model: model}, nil
}

// readRole reads the optional per-role override fields.  Missing keys are
// returned as empty strings; only a real database error causes a non-nil
// return.
func (g *Gateway) readRole(ctx context.Context, role Role) (Resolved, error) {
	prefix := fmt.Sprintf("roles.%s.", role)

	baseURL, err := g.getString(ctx, prefix+"baseURL")
	if err != nil {
		return Resolved{}, err
	}
	apiKey, err := g.getString(ctx, prefix+"apiKey")
	if err != nil {
		return Resolved{}, err
	}
	model, err := g.getString(ctx, prefix+"model")
	if err != nil {
		return Resolved{}, err
	}

	return Resolved{BaseURL: baseURL, APIKey: apiKey, Model: model}, nil
}

// retryLegalUnavailable reports whether the operator has opted in to retrying
// 451 Unavailable For Legal Reasons within the pre-first-token budget.  The
// key lives in this package's settings schema but is consumed by the
// resilience slice — this accessor only carries it, read-through, defaulting
// to false when unset or unparseable.  Write-time validation lives elsewhere.
func (g *Gateway) retryLegalUnavailable(ctx context.Context) (bool, error) {
	raw, err := g.getString(ctx, "retryLegalUnavailable")
	if err != nil {
		return false, err
	}
	if raw == "" {
		return false, nil
	}
	// Tolerant parse: a malformed value falls back to the default rather than
	// failing a turn, mirroring the lenient read-side stance of resolve.
	v, parseErr := strconv.ParseBool(raw)
	if parseErr != nil {
		return false, nil
	}
	return v, nil
}

// getString retrieves a single key from the settings reader, returning an
// empty string when the key is absent.  A real database error is wrapped with
// the key for context.
func (g *Gateway) getString(ctx context.Context, key string) (string, error) {
	val, ok, err := g.settings.Get(ctx, key)
	if err != nil {
		return "", fmt.Errorf("gateway: read setting %q: %w", key, err)
	}
	if !ok {
		return "", nil
	}
	return val, nil
}
