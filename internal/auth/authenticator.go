package auth

// authenticator.go implements httpx.TokenValidator by delegating to Sessions.Resolve. The clock is injected so tests
// can control time without real sleeps.

import (
	"context"
	"time"

	"github.com/qovira/qovira/internal/store"
)

// Authenticator implements [httpx.TokenValidator] by delegating token validation to [Sessions.Resolve].  Construct it
// via [NewAuthenticator] (uses [time.Now] as the clock) or [NewAuthenticatorWithClock] (test helper that injects a
// synthetic clock).
type Authenticator struct {
	sessions *Sessions
	now      func() time.Time
}

// NewAuthenticator returns an [Authenticator] backed by ss and using [time.Now] as the clock.  This is the production
// constructor.
func NewAuthenticator(ss *Sessions) *Authenticator {
	return &Authenticator{sessions: ss, now: time.Now}
}

// NewAuthenticatorWithClock returns an [Authenticator] backed by ss and using the provided now function as the clock.
// Intended for tests that need to inject a synthetic time to exercise TTL boundaries without real sleeps.
func NewAuthenticatorWithClock(ss *Sessions, now func() time.Time) *Authenticator {
	return &Authenticator{sessions: ss, now: now}
}

// ValidateToken implements [httpx.TokenValidator].  It delegates to [Sessions.Resolve] and translates
// [ErrSessionNotFound] (and any other resolution error) into a non-nil error so the auth middleware returns 401.
func (a *Authenticator) ValidateToken(ctx context.Context, token string) (store.Principal, error) {
	return a.sessions.Resolve(ctx, token, a.now())
}
