// Package bootstrap provides first-run detection and optional admin seeding for Qovira. It contains no CGO dependencies
// and no imports from internal/store, internal/config, or internal/cli — all concrete implementations are injected via
// the seam interfaces below, keeping this package fully unit-testable with in-process fakes.
//
// The composition root (internal/app) wires the concrete store and config values to these interfaces at server startup.
package bootstrap

import (
	"context"
	"fmt"
)

// KeyModelEndpoint is the settings key for the model gateway endpoint. The Model Gateway spec owns and writes this key;
// bootstrap only reads it to determine whether instance configuration is complete.
//
// This is the fully-qualified storage key. NeedsOnboarding must therefore read it through the un-namespaced settings
// store (whose Get looks up the literal key); passing a namespaced reader would prepend a prefix and look up the wrong
// key, silently reporting "needs onboarding" forever.
//
// Format: fully-qualified URL, e.g. "https://gateway.example.com/v1".
const KeyModelEndpoint = "model.endpoint"

// UserExister reports whether any user account exists in the system. Implementing this minimal interface avoids
// importing internal/store (which carries CGO/database dependencies) from this package.
type UserExister interface {
	// HasAnyUser returns (true, nil) when at least one user account exists, (false, nil) when none do, or (false, err)
	// on a store failure.
	HasAnyUser(ctx context.Context) (bool, error)
}

// AccountCreator creates a new admin account with the supplied credentials. Password hashing is the responsibility of
// the concrete implementation (Identity & Auth Spec) — this seam receives the plain-text password only so bootstrap
// never needs to know the hashing algorithm.
type AccountCreator interface {
	// CreateAdmin persists a new admin account identified by email and protected by password. It returns a non-nil
	// error on failure.
	CreateAdmin(ctx context.Context, email, password string) error
}

// SettingsReader is the read side of the instance settings store. It mirrors the signature of
// (*store.SettingsStore).Get exactly so the composition root can pass the real *store.SettingsStore without an adapter;
// the interface is defined here (at the consumer) to avoid importing internal/store.
type SettingsReader interface {
	// Get returns the value for key. It returns (value, true, nil) when the key exists, ("", false, nil) when absent,
	// and ("", false, err) on failure.
	Get(ctx context.Context, key string) (value string, found bool, err error)
}

// IsFirstRun returns true when no user accounts exist in the system, which defines "first run". It returns false when
// at least one account exists. Any store error is propagated unchanged; the bool is meaningless when err is non-nil,
// so callers must check err first.
func IsFirstRun(ctx context.Context, ue UserExister) (bool, error) {
	has, err := ue.HasAnyUser(ctx)
	if err != nil {
		return false, fmt.Errorf("bootstrap: check first run: %w", err)
	}
	return !has, nil
}

// NeedsOnboarding returns true when the instance is not yet fully configured. Configuration completeness is determined
// solely by whether a model endpoint has been written to the settings store — it is independent of whether an admin
// account exists. An absent key or an empty value both signal that onboarding is still required. Any store error is
// propagated unchanged; the bool is meaningless when err is non-nil, so callers must check err first.
func NeedsOnboarding(ctx context.Context, sr SettingsReader) (bool, error) {
	val, found, err := sr.Get(ctx, KeyModelEndpoint)
	if err != nil {
		return false, fmt.Errorf("bootstrap: check onboarding: %w", err)
	}
	return !found || val == "", nil
}

// MaybeSeedAdmin creates the initial admin account on first run when both adminEmail and adminPassword are non-empty.
// It is a no-op when any of the following conditions hold:
//   - isFirst is false (the system already has users)
//   - adminEmail is empty
//   - adminPassword is empty
//
// When seeding is performed, CreateAdmin is called exactly once. This function never writes any instance settings;
// needsOnboarding will still return true after a successful seed until the model endpoint is configured separately.
//
// The caller is responsible for resolving isFirst via IsFirstRun before calling this function. adminEmail and
// adminPassword should be passed from boot configuration (e.g. config.AdminEmail and string(config.AdminPassword))
// without importing internal/config here.
func MaybeSeedAdmin(
	ctx context.Context,
	isFirst bool,
	adminEmail, adminPassword string,
	ac AccountCreator,
) (seeded bool, err error) {
	if !isFirst || adminEmail == "" || adminPassword == "" {
		return false, nil
	}
	if err := ac.CreateAdmin(ctx, adminEmail, adminPassword); err != nil {
		return false, fmt.Errorf("bootstrap: seed admin: %w", err)
	}
	return true, nil
}
