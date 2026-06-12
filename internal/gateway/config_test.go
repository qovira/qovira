package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// fakeSettings is a pure in-memory settingsReader for use in tests.
// It is safe for concurrent use (protected by a mutex so -race is clean).
type fakeSettings struct {
	mu   sync.RWMutex
	data map[string]string
}

func newFakeSettings(pairs ...string) *fakeSettings {
	if len(pairs)%2 != 0 {
		panic("newFakeSettings: pairs must be even (key, value, key, value, …)")
	}
	fs := &fakeSettings{data: make(map[string]string, len(pairs)/2)}
	for i := 0; i < len(pairs); i += 2 {
		fs.data[pairs[i]] = pairs[i+1]
	}
	return fs
}

func (fs *fakeSettings) Get(_ context.Context, key string) (string, bool, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	v, ok := fs.data[key]
	return v, ok, nil
}

// set writes a key/value into the fake store (simulates a config change).
func (fs *fakeSettings) set(key, value string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.data[key] = value
}

// newGatewayWithFake constructs a Gateway backed by the provided fakeSettings.
// It bypasses New (which requires a *store.SettingsStore) so that tests stay
// CGO-free.
func newGatewayWithFake(fs settingsReader) *Gateway {
	return &Gateway{settings: fs}
}

// fullPrimary returns the fake settings keys that represent a fully-configured
// primary endpoint.
func fullPrimary(baseURL, apiKey, model string) []string {
	return []string{
		"primary.baseURL", baseURL,
		"primary.apiKey", apiKey,
		"primary.model", model,
	}
}

// ── retryLegalUnavailable: the carried opt-in key (consumed by the resilience slice) ─

func TestRetryLegalUnavailable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		keys []string
		want bool
	}{
		{name: "unset defaults to false", keys: nil, want: false},
		{name: "true", keys: []string{"retryLegalUnavailable", "true"}, want: true},
		{name: "false", keys: []string{"retryLegalUnavailable", "false"}, want: false},
		{name: "1 is true", keys: []string{"retryLegalUnavailable", "1"}, want: true},
		{name: "0 is false", keys: []string{"retryLegalUnavailable", "0"}, want: false},
		{name: "malformed falls back to false", keys: []string{"retryLegalUnavailable", "yes-please"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gw := newGatewayWithFake(newFakeSettings(tt.keys...))

			got, err := gw.retryLegalUnavailable(context.Background())
			if err != nil {
				t.Fatalf("retryLegalUnavailable: unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("retryLegalUnavailable = %v, want %v", got, tt.want)
			}
		})
	}
}

// ── AC1: resolve with only primary set returns primary verbatim for every role ─

func TestResolve_PrimaryOnly(t *testing.T) {
	t.Parallel()

	roles := []Role{RoleChat, RoleEmbeddings, RoleVision, RoleSTT, RoleTTS}

	for _, role := range roles {
		t.Run(string(role), func(t *testing.T) {
			t.Parallel()

			fs := newFakeSettings(fullPrimary("https://api.example.com", "sk-test", "gpt-4o")...)
			gw := newGatewayWithFake(fs)

			got, err := gw.resolve(context.Background(), role)
			if err != nil {
				t.Fatalf("resolve(%q): unexpected error: %v", role, err)
			}
			if got.BaseURL != "https://api.example.com" {
				t.Errorf("BaseURL = %q, want %q", got.BaseURL, "https://api.example.com")
			}
			if got.APIKey != "sk-test" {
				t.Errorf("APIKey = %q, want %q", got.APIKey, "sk-test")
			}
			if got.Model != "gpt-4o" {
				t.Errorf("Model = %q, want %q", got.Model, "gpt-4o")
			}
		})
	}
}

// ── AC2: partial override — only the set fields come from the override ─────────

func TestResolve_PartialOverride_ModelOnly(t *testing.T) {
	t.Parallel()

	keys := fullPrimary("https://primary.example.com", "sk-primary", "primary-model")
	keys = append(keys,
		"roles.chat.model", "chat-specific-model",
		// BaseURL and APIKey NOT set for the role — must fall through to primary.
	)
	fs := newFakeSettings(keys...)
	gw := newGatewayWithFake(fs)

	got, err := gw.resolve(context.Background(), RoleChat)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Model != "chat-specific-model" {
		t.Errorf("Model = %q, want %q (override)", got.Model, "chat-specific-model")
	}
	if got.BaseURL != "https://primary.example.com" {
		t.Errorf("BaseURL = %q, want %q (primary fallthrough)", got.BaseURL, "https://primary.example.com")
	}
	if got.APIKey != "sk-primary" {
		t.Errorf("APIKey = %q, want %q (primary fallthrough)", got.APIKey, "sk-primary")
	}
}

func TestResolve_PartialOverride_BaseURLAndAPIKey(t *testing.T) {
	t.Parallel()

	keys := fullPrimary("https://primary.example.com", "sk-primary", "base-model")
	keys = append(keys,
		"roles.embeddings.baseURL", "https://embed.example.com",
		"roles.embeddings.apiKey", "sk-embed",
		// Model NOT set — must fall through to primary.
	)
	fs := newFakeSettings(keys...)
	gw := newGatewayWithFake(fs)

	got, err := gw.resolve(context.Background(), RoleEmbeddings)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.BaseURL != "https://embed.example.com" {
		t.Errorf("BaseURL = %q, want %q (override)", got.BaseURL, "https://embed.example.com")
	}
	if got.APIKey != "sk-embed" {
		t.Errorf("APIKey = %q, want %q (override)", got.APIKey, "sk-embed")
	}
	if got.Model != "base-model" {
		t.Errorf("Model = %q, want %q (primary fallthrough)", got.Model, "base-model")
	}
}

// ── AC3: full override fully shadows primary for that role ────────────────────

func TestResolve_FullOverride_ShadowsPrimary(t *testing.T) {
	t.Parallel()

	keys := fullPrimary("https://primary.example.com", "sk-primary", "primary-model")
	keys = append(keys,
		"roles.vision.baseURL", "https://vision.example.com",
		"roles.vision.apiKey", "sk-vision",
		"roles.vision.model", "vision-model",
	)
	fs := newFakeSettings(keys...)
	gw := newGatewayWithFake(fs)

	got, err := gw.resolve(context.Background(), RoleVision)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.BaseURL != "https://vision.example.com" {
		t.Errorf("BaseURL = %q, want %q", got.BaseURL, "https://vision.example.com")
	}
	if got.APIKey != "sk-vision" {
		t.Errorf("APIKey = %q, want %q", got.APIKey, "sk-vision")
	}
	if got.Model != "vision-model" {
		t.Errorf("Model = %q, want %q", got.Model, "vision-model")
	}
}

// FullOverride must not affect other roles; they still see primary values.
func TestResolve_FullOverride_DoesNotAffectOtherRoles(t *testing.T) {
	t.Parallel()

	keys := fullPrimary("https://primary.example.com", "sk-primary", "primary-model")
	keys = append(keys,
		"roles.vision.baseURL", "https://vision.example.com",
		"roles.vision.apiKey", "sk-vision",
		"roles.vision.model", "vision-model",
	)
	fs := newFakeSettings(keys...)
	gw := newGatewayWithFake(fs)

	got, err := gw.resolve(context.Background(), RoleChat)
	if err != nil {
		t.Fatalf("resolve(chat): %v", err)
	}
	if got.BaseURL != "https://primary.example.com" {
		t.Errorf("chat BaseURL = %q, want %q (primary)", got.BaseURL, "https://primary.example.com")
	}
	if got.Model != "primary-model" {
		t.Errorf("chat Model = %q, want %q (primary)", got.Model, "primary-model")
	}
}

// ── AC4: no primary → ErrGatewayNotConfigured ────────────────────────────────

func TestResolve_NoPrimary_ErrGatewayNotConfigured(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		keys []string // primary keys present (zero or partial)
	}{
		{
			name: "no_keys_at_all",
			keys: nil,
		},
		{
			name: "missing_model",
			keys: []string{
				"primary.baseURL", "https://api.example.com",
				"primary.apiKey", "sk-test",
				// model absent
			},
		},
		{
			name: "missing_apiKey",
			keys: []string{
				"primary.baseURL", "https://api.example.com",
				"primary.model", "gpt-4o",
				// apiKey absent
			},
		},
		{
			name: "missing_baseURL",
			keys: []string{
				"primary.apiKey", "sk-test",
				"primary.model", "gpt-4o",
				// baseURL absent
			},
		},
		{
			name: "empty_model",
			keys: []string{
				"primary.baseURL", "https://api.example.com",
				"primary.apiKey", "sk-test",
				"primary.model", "", // empty string counts as absent
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var fs *fakeSettings
			if len(tt.keys) > 0 {
				fs = newFakeSettings(tt.keys...)
			} else {
				fs = newFakeSettings()
			}
			gw := newGatewayWithFake(fs)

			_, err := gw.resolve(context.Background(), RoleChat)
			if !errors.Is(err, ErrGatewayNotConfigured) {
				t.Errorf("resolve = %v, want ErrGatewayNotConfigured", err)
			}
		})
	}
}

// ── AC5: config change reflected by the very next resolve call ───────────────

func TestResolve_ConfigChangeReflectedImmediately(t *testing.T) {
	t.Parallel()

	// Start with a fully-configured primary.
	fs := newFakeSettings(fullPrimary("https://v1.example.com", "sk-v1", "model-v1")...)
	gw := newGatewayWithFake(fs)
	ctx := context.Background()

	// First resolve: returns v1 values.
	got, err := gw.resolve(ctx, RoleChat)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if got.BaseURL != "https://v1.example.com" {
		t.Errorf("before update: BaseURL = %q, want %q", got.BaseURL, "https://v1.example.com")
	}

	// Simulate a config write (no restart — the same Gateway instance).
	fs.set("primary.baseURL", "https://v2.example.com")
	fs.set("primary.model", "model-v2")

	// Next resolve must reflect the new values immediately (read-through).
	got, err = gw.resolve(ctx, RoleChat)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if got.BaseURL != "https://v2.example.com" {
		t.Errorf("after update: BaseURL = %q, want %q", got.BaseURL, "https://v2.example.com")
	}
	if got.Model != "model-v2" {
		t.Errorf("after update: Model = %q, want %q", got.Model, "model-v2")
	}
	// APIKey was not changed.
	if got.APIKey != "sk-v1" {
		t.Errorf("after update: APIKey = %q, want %q (unchanged)", got.APIKey, "sk-v1")
	}
}

// ── AC6: table-driven test covering all four scenarios ────────────────────────

func TestResolve_TableDriven(t *testing.T) {
	t.Parallel()

	type setup struct {
		keys []string // flat key/value pairs for newFakeSettings
	}

	tests := []struct {
		name    string
		setup   setup
		role    Role
		want    Resolved
		wantErr error
	}{
		{
			name:  "all_default_chat",
			setup: setup{keys: fullPrimary("https://api.example.com", "sk-api", "gpt-4o")},
			role:  RoleChat,
			want:  Resolved{BaseURL: "https://api.example.com", APIKey: "sk-api", Model: "gpt-4o"},
		},
		{
			name:  "all_default_embeddings",
			setup: setup{keys: fullPrimary("https://api.example.com", "sk-api", "text-embed-3")},
			role:  RoleEmbeddings,
			want:  Resolved{BaseURL: "https://api.example.com", APIKey: "sk-api", Model: "text-embed-3"},
		},
		{
			name: "partial_override_model_only",
			setup: setup{keys: append(
				fullPrimary("https://primary.example.com", "sk-primary", "base-model"),
				"roles.chat.model", "chat-model",
			)},
			role: RoleChat,
			want: Resolved{
				BaseURL: "https://primary.example.com",
				APIKey:  "sk-primary",
				Model:   "chat-model",
			},
		},
		{
			name: "partial_override_base_url_and_api_key",
			setup: setup{keys: append(
				fullPrimary("https://primary.example.com", "sk-primary", "base-model"),
				"roles.stt.baseURL", "https://stt.example.com",
				"roles.stt.apiKey", "sk-stt",
			)},
			role: RoleSTT,
			want: Resolved{
				BaseURL: "https://stt.example.com",
				APIKey:  "sk-stt",
				Model:   "base-model",
			},
		},
		{
			name: "full_override",
			setup: setup{keys: append(
				fullPrimary("https://primary.example.com", "sk-primary", "primary-model"),
				"roles.tts.baseURL", "https://tts.example.com",
				"roles.tts.apiKey", "sk-tts",
				"roles.tts.model", "tts-model",
			)},
			role: RoleTTS,
			want: Resolved{
				BaseURL: "https://tts.example.com",
				APIKey:  "sk-tts",
				Model:   "tts-model",
			},
		},
		{
			name:    "not_configured_no_keys",
			setup:   setup{keys: nil},
			role:    RoleChat,
			wantErr: ErrGatewayNotConfigured,
		},
		{
			name: "not_configured_partial_primary",
			setup: setup{keys: []string{
				"primary.baseURL", "https://api.example.com",
				// apiKey and model absent
			}},
			role:    RoleEmbeddings,
			wantErr: ErrGatewayNotConfigured,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var fs *fakeSettings
			if len(tt.setup.keys) > 0 {
				fs = newFakeSettings(tt.setup.keys...)
			} else {
				fs = newFakeSettings()
			}
			gw := newGatewayWithFake(fs)

			got, err := gw.resolve(context.Background(), tt.role)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("resolve error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolve = %+v, want %+v", got, tt.want)
			}
		})
	}
}
