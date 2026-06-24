package auth_test

// Tests for Sessions.Resolve and the Authenticator adapter.
//
// Uses the same store test harness as sessions_test.go (openSessionsStore, createTestUser, testSessionConfig). The
// clock is injected via explicit now parameters so tests never need real sleeps.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/auth"
)

// ── Sessions.Resolve ──────────────────────────────────────────────────────────

// TestResolve_ValidToken_ReturnsPrincipal verifies that Resolve returns store.Principal{UserID, Role} for a valid,
// in-window token.
func TestResolve_ValidToken_ReturnsPrincipal(t *testing.T) {
	t.Parallel()

	_, svc, sessions := openSessionsStore(t)
	ctx := context.Background()
	u := createTestUser(t, svc, "resolve-valid@example.com")
	now := time.Now().UTC()

	token, _, _, err := sessions.Mint(ctx, u.ID, now)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	principal, err := sessions.Resolve(ctx, token, now)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if principal.UserID != u.ID {
		t.Errorf("principal.UserID = %q, want %q", principal.UserID, u.ID)
	}
	if principal.Role != string(auth.RoleMember) {
		t.Errorf("principal.Role = %q, want %q", principal.Role, string(auth.RoleMember))
	}
}

// TestResolve_AdminRole_ReturnsPrincipalWithAdminRole verifies that when a user has the admin role the returned
// principal carries "admin".
func TestResolve_AdminRole_ReturnsPrincipalWithAdminRole(t *testing.T) {
	t.Parallel()

	_, svc, sessions := openSessionsStore(t)
	ctx := context.Background()

	adminUser, err := svc.CreateUser(ctx, auth.NewUser{
		Email:       "resolve-admin@example.com",
		DisplayName: "Admin User",
		Password:    "correct-horse",
		Role:        auth.RoleAdmin,
		Timezone:    "UTC",
		Locale:      "en-US",
		Language:    "en",
	})
	if err != nil {
		t.Fatalf("CreateUser (admin): %v", err)
	}

	now := time.Now().UTC()
	token, _, _, err := sessions.Mint(ctx, adminUser.ID, now)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	principal, err := sessions.Resolve(ctx, token, now)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if principal.Role != string(auth.RoleAdmin) {
		t.Errorf("principal.Role = %q, want %q", principal.Role, string(auth.RoleAdmin))
	}
}

// TestResolve_UnknownToken_ReturnsNotFound verifies that an unrecognised token returns ErrSessionNotFound.
func TestResolve_UnknownToken_ReturnsNotFound(t *testing.T) {
	t.Parallel()

	_, _, sessions := openSessionsStore(t)
	ctx := context.Background()

	_, err := sessions.Resolve(ctx, "qov_notarealtokennnnnnnnnnnnnnnnnnnnnnnnnnnnnn", time.Now().UTC())
	if !errors.Is(err, auth.ErrSessionNotFound) {
		t.Errorf("Resolve (unknown token) = %v, want ErrSessionNotFound", err)
	}
}

// TestResolve_ExpiredSession_ReturnsNotFoundAndDeleted verifies that an expired session returns ErrSessionNotFound and
// the row is best-effort deleted so a subsequent Lookup also misses.
func TestResolve_ExpiredSession_ReturnsNotFoundAndDeleted(t *testing.T) {
	t.Parallel()

	_, svc, sessions := openSessionsStore(t)
	ctx := context.Background()
	u := createTestUser(t, svc, "resolve-expired@example.com")

	// Mint at a time that puts the session past the idle TTL.
	mintNow := time.Now().UTC()
	token, _, _, err := sessions.Mint(ctx, u.ID, mintNow)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Advance now past the idle TTL.
	resolveNow := mintNow.Add(testSessionConfig.IdleTTL + time.Second)

	_, err = sessions.Resolve(ctx, token, resolveNow)
	if !errors.Is(err, auth.ErrSessionNotFound) {
		t.Errorf("Resolve (expired) = %v, want ErrSessionNotFound", err)
	}

	// Best-effort delete should have removed the row.
	_, lookupErr := sessions.Lookup(ctx, token)
	if !errors.Is(lookupErr, auth.ErrSessionNotFound) {
		t.Errorf("Lookup after Resolve-expired = %v, want ErrSessionNotFound (row should be deleted)", lookupErr)
	}
}

// TestResolve_ValidSession_BumpThrottled verifies that a bump within BumpInterval is silently skipped — the request
// still succeeds and returns the principal.
func TestResolve_ValidSession_BumpThrottled(t *testing.T) {
	t.Parallel()

	_, svc, sessions := openSessionsStore(t)
	ctx := context.Background()
	u := createTestUser(t, svc, "resolve-bump-throttle@example.com")
	now := time.Now().UTC()

	token, sess, _, err := sessions.Mint(ctx, u.ID, now)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Resolve within BumpInterval — bump should be skipped (no error).
	resolveNow := now.Add(testSessionConfig.BumpInterval - time.Second)
	principal, err := sessions.Resolve(ctx, token, resolveNow)
	if err != nil {
		t.Fatalf("Resolve (within BumpInterval): %v", err)
	}
	if principal.UserID != u.ID {
		t.Errorf("principal.UserID = %q, want %q", principal.UserID, u.ID)
	}

	// last_used_at must be unchanged (bump was a no-op).
	found, err := sessions.Lookup(ctx, token)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !found.LastUsedAt.Equal(sess.LastUsedAt) {
		t.Errorf("LastUsedAt changed inside BumpInterval: was %v, now %v", sess.LastUsedAt, found.LastUsedAt)
	}
}

// TestResolve_ValidSession_BumpAfterInterval verifies that after BumpInterval the Resolve call issues a bump without
// failing the request.
func TestResolve_ValidSession_BumpAfterInterval(t *testing.T) {
	t.Parallel()

	_, svc, sessions := openSessionsStore(t)
	ctx := context.Background()
	u := createTestUser(t, svc, "resolve-bump-write@example.com")
	now := time.Now().UTC()

	token, _, _, err := sessions.Mint(ctx, u.ID, now)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Resolve after BumpInterval — bump should fire.
	resolveNow := now.Add(testSessionConfig.BumpInterval + time.Second)
	principal, err := sessions.Resolve(ctx, token, resolveNow)
	if err != nil {
		t.Fatalf("Resolve (after BumpInterval): %v", err)
	}
	if principal.UserID != u.ID {
		t.Errorf("principal.UserID = %q, want %q", principal.UserID, u.ID)
	}

	// last_used_at must be updated.
	found, err := sessions.Lookup(ctx, token)
	if err != nil {
		t.Fatalf("Lookup after bump: %v", err)
	}
	wantLastUsedAt := resolveNow.Truncate(time.Second)
	if !found.LastUsedAt.Equal(wantLastUsedAt) {
		t.Errorf("LastUsedAt after bump = %v, want %v", found.LastUsedAt, wantLastUsedAt)
	}
}

// ── Concurrency / race safety ─────────────────────────────────────────────────

// TestResolve_ConcurrentResolve verifies that concurrent Resolve calls against a shared *Sessions are race-free.
func TestResolve_ConcurrentResolve(t *testing.T) {
	t.Parallel()

	_, svc, sessions := openSessionsStore(t)
	ctx := context.Background()
	u := createTestUser(t, svc, "concurrent-resolve@example.com")
	now := time.Now().UTC()

	// Mint a few tokens to give goroutines something to resolve.
	const numTokens = 4
	tokens := make([]string, numTokens)
	for i := range tokens {
		tok, _, _, err := sessions.Mint(ctx, u.ID, now)
		if err != nil {
			t.Fatalf("Mint %d: %v", i, err)
		}
		tokens[i] = tok
	}

	var wg sync.WaitGroup
	const goroutines = 16
	resolveNow := now.Add(testSessionConfig.BumpInterval + time.Second)

	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			tok := tokens[g%numTokens]
			_, err := sessions.Resolve(ctx, tok, resolveNow)
			if err != nil {
				t.Errorf("goroutine %d Resolve: %v", g, err)
			}
		}(g)
	}

	wg.Wait()
}

// TestResolve_ConcurrentDeleteAndResolve verifies that DeleteAllForUser racing an in-flight Resolve is clean: the
// Resolve may return either a valid principal (if it beats the delete) or ErrSessionNotFound (if the delete wins). No
// panic or data race must occur.
func TestResolve_ConcurrentDeleteAndResolve(t *testing.T) {
	t.Parallel()

	_, svc, sessions := openSessionsStore(t)
	ctx := context.Background()
	u := createTestUser(t, svc, "concurrent-delete-resolve@example.com")
	now := time.Now().UTC()

	token, _, _, err := sessions.Mint(ctx, u.ID, now)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_ = sessions.DeleteAllForUser(ctx, u.ID)
	}()

	go func() {
		defer wg.Done()
		_, err := sessions.Resolve(ctx, token, now)
		// Either a valid principal or ErrSessionNotFound — both are acceptable.
		if err != nil && !errors.Is(err, auth.ErrSessionNotFound) {
			t.Errorf("Resolve: unexpected error: %v", err)
		}
	}()

	wg.Wait()
}

// ── Authenticator adapter ─────────────────────────────────────────────────────

// TestAuthenticator_ValidToken_ReturnsPrincipal verifies that the Authenticator adapts ValidateToken to Resolve and
// returns the correct Principal.
func TestAuthenticator_ValidToken_ReturnsPrincipal(t *testing.T) {
	t.Parallel()

	_, svc, sessions := openSessionsStore(t)
	ctx := context.Background()
	u := createTestUser(t, svc, "authn-valid@example.com")

	mintNow := time.Now().UTC()
	token, _, _, err := sessions.Mint(ctx, u.ID, mintNow)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Build an Authenticator with a fixed clock so Resolve sees now == mintNow.
	a := auth.NewAuthenticatorWithClock(sessions, func() time.Time { return mintNow })

	principal, err := a.ValidateToken(ctx, token)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if principal.UserID != u.ID {
		t.Errorf("principal.UserID = %q, want %q", principal.UserID, u.ID)
	}
	if principal.Role != string(auth.RoleMember) {
		t.Errorf("principal.Role = %q, want %q", principal.Role, string(auth.RoleMember))
	}
}

// TestAuthenticator_InvalidToken_ReturnsError verifies that an unrecognised token yields a non-nil error from
// ValidateToken.
func TestAuthenticator_InvalidToken_ReturnsError(t *testing.T) {
	t.Parallel()

	_, _, sessions := openSessionsStore(t)
	ctx := context.Background()

	a := auth.NewAuthenticator(sessions)

	_, err := a.ValidateToken(ctx, "qov_notarealtokennnnnnnnnnnnnnnnnnnnnnnnnnnnnn")
	if err == nil {
		t.Error("ValidateToken (invalid token) = nil error, want non-nil")
	}
}

// TestAuthenticator_ExpiredToken_ReturnsError verifies that an expired session yields an error from ValidateToken.
func TestAuthenticator_ExpiredToken_ReturnsError(t *testing.T) {
	t.Parallel()

	_, svc, sessions := openSessionsStore(t)
	ctx := context.Background()
	u := createTestUser(t, svc, "authn-expired@example.com")

	mintNow := time.Now().UTC()
	token, _, _, err := sessions.Mint(ctx, u.ID, mintNow)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Clock reports past the idle TTL.
	expiredNow := mintNow.Add(testSessionConfig.IdleTTL + time.Second)
	a := auth.NewAuthenticatorWithClock(sessions, func() time.Time { return expiredNow })

	_, err = a.ValidateToken(ctx, token)
	if err == nil {
		t.Error("ValidateToken (expired) = nil error, want non-nil")
	}
}
