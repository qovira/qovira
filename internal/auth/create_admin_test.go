package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/qovira/qovira/internal/auth"
)

// TestCreateAdmin_CreatesAdminWithCorrectRole verifies that CreateAdmin creates a user with role "admin", a display
// name derived from the email local-part, and that the password authenticates successfully via Authenticate.
func TestCreateAdmin_CreatesAdminWithCorrectRole(t *testing.T) {
	t.Parallel()

	_, svc := openServiceStore(t)
	ctx := context.Background()

	const email = "admin@example.com"
	const password = "correct-horse-admin"

	if err := svc.CreateAdmin(ctx, email, password); err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}

	// The user must exist with role admin.
	user, err := svc.GetUserByEmail(ctx, email)
	if err != nil {
		t.Fatalf("GetUserByEmail after CreateAdmin: %v", err)
	}
	if user.Role != auth.RoleAdmin {
		t.Errorf("Role = %q, want %q", user.Role, auth.RoleAdmin)
	}
	if user.Email != "admin@example.com" {
		t.Errorf("Email = %q, want %q", user.Email, "admin@example.com")
	}

	// The password must authenticate correctly via Authenticate.
	got, err := svc.Authenticate(ctx, email, password)
	if err != nil {
		t.Fatalf("Authenticate after CreateAdmin: %v", err)
	}
	if got.Role != auth.RoleAdmin {
		t.Errorf("Authenticate: Role = %q, want %q", got.Role, auth.RoleAdmin)
	}
}

// TestCreateAdmin_DisplayNameDerivedFromEmail verifies that the display name is derived from the local-part of the
// email address (the part before '@').
func TestCreateAdmin_DisplayNameDerivedFromEmail(t *testing.T) {
	t.Parallel()

	_, svc := openServiceStore(t)
	ctx := context.Background()

	if err := svc.CreateAdmin(ctx, "ada.lovelace@example.com", "correct-horse-123"); err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}

	user, err := svc.GetUserByEmail(ctx, "ada.lovelace@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	// Display name must be the local-part (before @).
	if user.DisplayName != "ada.lovelace" {
		t.Errorf("DisplayName = %q, want %q", user.DisplayName, "ada.lovelace")
	}
}

// TestCreateAdmin_SaneProfileDefaults verifies that the default profile fields (timezone, locale, language) are
// populated with sane values.
func TestCreateAdmin_SaneProfileDefaults(t *testing.T) {
	t.Parallel()

	_, svc := openServiceStore(t)
	ctx := context.Background()

	if err := svc.CreateAdmin(ctx, "defaults@example.com", "correct-horse-456"); err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}

	user, err := svc.GetUserByEmail(ctx, "defaults@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if user.Timezone == "" {
		t.Error("Timezone is empty; want a non-empty default (e.g. UTC)")
	}
	if user.Locale == "" {
		t.Error("Locale is empty; want a non-empty default (e.g. en)")
	}
	if user.Language == "" {
		t.Error("Language is empty; want a non-empty default (e.g. en)")
	}
}

// TestCreateAdmin_DuplicateEmail_ReturnsErrEmailTaken verifies that calling CreateAdmin a second time for the same
// email returns ErrEmailTaken.
func TestCreateAdmin_DuplicateEmail_ReturnsErrEmailTaken(t *testing.T) {
	t.Parallel()

	_, svc := openServiceStore(t)
	ctx := context.Background()

	if err := svc.CreateAdmin(ctx, "dup@example.com", "correct-horse-789"); err != nil {
		t.Fatalf("first CreateAdmin: %v", err)
	}

	err := svc.CreateAdmin(ctx, "dup@example.com", "correct-horse-789")
	if !errors.Is(err, auth.ErrEmailTaken) {
		t.Errorf("second CreateAdmin same email = %v, want ErrEmailTaken", err)
	}
}

// TestCreateAdmin_ImplementsAccountCreatorSeam verifies that *auth.Service satisfies the bootstrap.AccountCreator
// interface at compile time by using an interface conversion. The actual interface lives in bootstrap; we replicate the
// minimal shape here to avoid a circular import.
func TestCreateAdmin_ImplementsAccountCreatorSeam(t *testing.T) {
	t.Parallel()

	type accountCreator interface {
		CreateAdmin(ctx context.Context, email, password string) error
	}

	_, svc := openServiceStore(t)
	var _ accountCreator = svc
}
