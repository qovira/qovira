package auth

import (
	"context"
	"fmt"
	"strings"
)

// CreateAdmin creates a new admin account with the supplied email and plaintext password. It is intended for
// first-run seeding via the composition root; production callers should prefer [Service.CreateUser] for full control
// over all fields.
//
// Profile defaults applied:
//   - DisplayName: the local-part of email (the segment before '@').
//   - Timezone:    "UTC" — a safe, universally valid IANA zone.
//   - Locale:      "en"  — BCP 47 base language tag.
//   - Language:    "en"  — BCP 47 base language tag.
//
// Password hashing and policy validation are delegated to [Service.CreateUser], so the same argon2id params and policy
// apply here.
//
// CreateAdmin satisfies the bootstrap.AccountCreator seam, allowing the composition root to pass the *Service directly
// without an adapter.
func (s *Service) CreateAdmin(ctx context.Context, email, password string) error {
	displayName := displayNameFromEmail(email)
	_, err := s.CreateUser(ctx, NewUser{
		Email:       email,
		DisplayName: displayName,
		Password:    password,
		Role:        RoleAdmin,
		Timezone:    "UTC",
		Locale:      "en",
		Language:    "en",
	})
	if err != nil {
		return fmt.Errorf("auth: create admin %q: %w", email, err)
	}
	return nil
}

// displayNameFromEmail returns the local-part of an email address (the segment before the first '@'). If '@' is absent,
// the full string is returned.
func displayNameFromEmail(email string) string {
	local, _, found := strings.Cut(email, "@")
	if !found {
		return email
	}
	return local
}
