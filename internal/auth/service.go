package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	sqlite3 "github.com/omnilium/go-sqlcipher"

	"github.com/qovira/qovira/internal/id"
	"github.com/qovira/qovira/internal/store"
	"github.com/qovira/qovira/internal/store/db"
)

// ── Sentinel errors ───────────────────────────────────────────────────────────

// ErrEmailTaken is returned by [Service.CreateUser] when the normalised email
// already exists in the users table.  Use [errors.Is] to check.
var ErrEmailTaken = errors.New("email already taken")

// ErrInvalidCredentials is returned by [Service.Authenticate] when the email
// is not found or the password does not match.  The same sentinel is used for
// both cases deliberately — callers must not be able to distinguish an unknown
// email from a wrong password (user-enumeration prevention).  Use [errors.Is]
// to check.
var ErrInvalidCredentials = errors.New("invalid credentials")

// ErrUserNotFound is returned by [Service.GetUserByEmail], [Service.GetUserByID],
// [Service.UpdateProfile], and [Service.UpdatePasswordHash] when no matching row
// exists.  Use [errors.Is] to check.
var ErrUserNotFound = errors.New("user not found")

// ErrInvalidRole is returned by [Service.CreateUser] when Role is not one of
// the two recognised values.  Use [errors.Is] to check.
var ErrInvalidRole = errors.New("invalid role: must be 'admin' or 'member'")

// ── Role ─────────────────────────────────────────────────────────────────────

// Role is the set of values a user account may have.
type Role string

const (
	// RoleAdmin is the admin role.  Admins have full system access.
	RoleAdmin Role = "admin"
	// RoleMember is the standard user role.
	RoleMember Role = "member"
)

// valid reports whether r is a recognised Role value.
func (r Role) valid() bool {
	return r == RoleAdmin || r == RoleMember
}

// ── User ─────────────────────────────────────────────────────────────────────

// User is the safe user record returned to callers.  It deliberately omits
// PasswordHash to prevent accidental leakage into logs or API responses.
// To access the stored hash for authentication purposes, use the package-private
// [Service.userRowByEmail] or [Service.userRowByID] helpers that return the raw
// [db.User] (hash included).
type User struct {
	ID          string
	Email       string
	DisplayName string
	Role        Role
	Timezone    string
	Locale      string
	Language    string
	CreatedAt   string
	UpdatedAt   string
}

// userFromRow converts a generated [db.User] (which includes PasswordHash) into
// the safe public [User] type.  It is the only place this conversion happens so
// hash omission is enforced structurally.
func userFromRow(row db.User) User {
	return User{
		ID:          row.ID,
		Email:       row.Email,
		DisplayName: row.DisplayName,
		Role:        Role(row.Role),
		Timezone:    row.Timezone,
		Locale:      row.Locale,
		Language:    row.Language,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}
}

// ── NewUser ───────────────────────────────────────────────────────────────────

// NewUser carries the fields required to create a new user account.
type NewUser struct {
	Email       string
	DisplayName string
	Password    string // plain-text; validated by Policy and hashed before storage
	Role        Role
	Timezone    string
	Locale      string
	Language    string
}

// ── Service ───────────────────────────────────────────────────────────────────

// Service provides the user-management operations for the Qovira identity layer.
// Construct it via [NewService]; the zero value is not valid.
//
// The Service owns the CREATE/READ/UPDATE paths for the users table; it does not
// expose raw password hashes through any public method.
type Service struct {
	readQ  *db.Queries
	writeQ *db.Queries
	hasher *Hasher
	policy Policy
}

// NewService constructs a Service backed by the provided store and credential
// components.  Reads go through the read pool; writes go through the write pool.
func NewService(s *store.Store, h *Hasher, p Policy) *Service {
	return &Service{
		readQ:  db.New(s.Reader()),
		writeQ: db.New(s.Writer()),
		hasher: h,
		policy: p,
	}
}

// ── normalizeEmail ────────────────────────────────────────────────────────────

// normalizeEmail trims surrounding whitespace and lower-cases the email.  It is
// applied before every INSERT and every lookup so that the UNIQUE constraint
// enforces case-insensitive uniqueness and lookups are stable regardless of what
// the caller supplies.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ── CreateUser ────────────────────────────────────────────────────────────────

// CreateUser validates, hashes, and persists a new user account.  It returns the
// safe [User] record (no PasswordHash) on success.
//
// Order of operations:
//  1. Normalise email.
//  2. Validate password against [Policy] — returns [ErrPasswordTooShort] /
//     [ErrPasswordTooLong] before any DB work.
//  3. Validate Role.
//  4. Hash the password via argon2id.
//  5. INSERT into users.
//  6. On UNIQUE constraint violation (email already taken) → [ErrEmailTaken].
func (s *Service) CreateUser(ctx context.Context, in NewUser) (User, error) {
	email := normalizeEmail(in.Email)

	if err := s.policy.ValidatePassword(in.Password); err != nil {
		return User{}, err
	}

	if !in.Role.valid() {
		return User{}, ErrInvalidRole
	}

	phc, err := s.hasher.Hash(in.Password)
	if err != nil {
		return User{}, fmt.Errorf("auth: hash password: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	params := db.CreateUserParams{
		ID:           id.New(),
		Email:        email,
		DisplayName:  in.DisplayName,
		PasswordHash: phc,
		Role:         string(in.Role),
		Timezone:     in.Timezone,
		Locale:       in.Locale,
		Language:     in.Language,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if err := s.writeQ.CreateUser(ctx, params); err != nil {
		if isUniqueConstraintError(err) {
			return User{}, ErrEmailTaken
		}
		return User{}, fmt.Errorf("auth: create user: %w", err)
	}

	// Read back the row so the returned User carries the stored values (e.g. ID).
	row, err := s.readQ.GetUserByID(ctx, params.ID)
	if err != nil {
		return User{}, fmt.Errorf("auth: read back created user: %w", err)
	}
	return userFromRow(row), nil
}

// ── GetUserByEmail ────────────────────────────────────────────────────────────

// GetUserByEmail looks up a user by their email address.  The email is
// normalised before lookup.  Returns [ErrUserNotFound] when no row matches.
func (s *Service) GetUserByEmail(ctx context.Context, email string) (User, error) {
	row, err := s.readQ.GetUserByEmail(ctx, normalizeEmail(email))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrUserNotFound
		}
		return User{}, fmt.Errorf("auth: get user by email: %w", err)
	}
	return userFromRow(row), nil
}

// ── GetUserByID ───────────────────────────────────────────────────────────────

// GetUserByID looks up a user by their ULID.  Returns [ErrUserNotFound] when no
// row matches.
func (s *Service) GetUserByID(ctx context.Context, userID string) (User, error) {
	row, err := s.readQ.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrUserNotFound
		}
		return User{}, fmt.Errorf("auth: get user by id: %w", err)
	}
	return userFromRow(row), nil
}

// ── UpdateProfile ─────────────────────────────────────────────────────────────

// UpdateProfile replaces the display_name, timezone, locale, and language for
// the user identified by userID.  updated_at is always bumped.  Returns
// [ErrUserNotFound] when no row with that ID exists.
func (s *Service) UpdateProfile(ctx context.Context, userID, displayName, timezone, locale, language string) error {
	n, err := s.writeQ.UpdateUserProfile(ctx, db.UpdateUserProfileParams{
		ID:          userID,
		DisplayName: displayName,
		Timezone:    timezone,
		Locale:      locale,
		Language:    language,
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("auth: update profile: %w", err)
	}
	if n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// ── UpdatePasswordHash ────────────────────────────────────────────────────────

// UpdatePasswordHash replaces the stored argon2id PHC hash for userID.  The
// caller is responsible for supplying a valid PHC string (e.g. produced by
// [Hasher.Hash]).  updated_at is bumped.  Returns [ErrUserNotFound] when no
// row with that ID exists.
//
// This method accepts a pre-computed PHC string rather than a plaintext password
// because the login/reset slice (QOV-54) owns the policy validation and
// re-hashing decision; this slice is responsible only for the storage update.
func (s *Service) UpdatePasswordHash(ctx context.Context, userID, phc string) error {
	n, err := s.writeQ.UpdateUserPasswordHash(ctx, db.UpdateUserPasswordHashParams{
		ID:           userID,
		PasswordHash: phc,
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("auth: update password hash: %w", err)
	}
	if n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// ── userRowByEmail ────────────────────────────────────────────────────────────

// userRowByEmail fetches the raw [db.User] row (including PasswordHash) for the
// given email after normalisation.  It is package-private and used only by
// [Service.Authenticate] so the hash is never reachable through a public method.
// Returns [sql.ErrNoRows] when no row matches.
func (s *Service) userRowByEmail(ctx context.Context, email string) (db.User, error) {
	row, err := s.readQ.GetUserByEmail(ctx, normalizeEmail(email))
	if err != nil {
		return db.User{}, err
	}
	return row, nil
}

// ── userRowByID ───────────────────────────────────────────────────────────────

// userRowByID fetches the raw [db.User] row (including PasswordHash) for the
// given user ID.  It is package-private and used only by [Service.ChangePassword]
// so the hash is never reachable through a public method.
// Returns [sql.ErrNoRows] when no row matches.
func (s *Service) userRowByID(ctx context.Context, userID string) (db.User, error) {
	row, err := s.readQ.GetUserByID(ctx, userID)
	if err != nil {
		return db.User{}, err
	}
	return row, nil
}

// ── ChangePassword ────────────────────────────────────────────────────────────

// ChangePassword verifies the caller's current password, validates the new one
// against the policy, and updates the stored hash.  Session revocation is the
// caller's responsibility — this method has no concept of a current session ID.
//
// Order of operations:
//  1. Load the raw DB row by userID; [sql.ErrNoRows] → [ErrUserNotFound].
//  2. [Hasher.Verify] the stored PHC against currentPassword.
//     Mismatch → [ErrInvalidCredentials].
//     Malformed-PHC / infrastructure error from Verify → wrapped error (not
//     [ErrInvalidCredentials]; the caller can distinguish by checking
//     errors.Is(err, ErrInvalidCredentials)).
//  3. [Policy.ValidatePassword] on newPassword; failure → the policy sentinel
//     unchanged ([ErrPasswordTooShort] or [ErrPasswordTooLong]).
//  4. [Hasher.Hash] the new password and call [Service.UpdatePasswordHash].
//
// The ordering of steps 2 before 3 is intentional: a wrong current password is
// reported even when the new password is also policy-violating.
func (s *Service) ChangePassword(ctx context.Context, userID, currentPassword, newPassword string) error {
	row, err := s.userRowByID(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		return fmt.Errorf("auth: change password lookup: %w", err)
	}

	ok, err := s.hasher.Verify(row.PasswordHash, currentPassword)
	if err != nil {
		// Malformed PHC or infrastructure error — wrap and return; the caller
		// checks errors.Is(err, ErrInvalidCredentials) and this is NOT that.
		return fmt.Errorf("auth: verify current password hash: %w", err)
	}
	if !ok {
		return ErrInvalidCredentials
	}

	if err := s.policy.ValidatePassword(newPassword); err != nil {
		return err
	}

	newPHC, err := s.hasher.Hash(newPassword)
	if err != nil {
		return fmt.Errorf("auth: hash new password: %w", err)
	}

	return s.UpdatePasswordHash(ctx, userID, newPHC)
}

// ── Authenticate ──────────────────────────────────────────────────────────────

// Authenticate validates email/password credentials and returns the safe [User]
// on success.  Both the unknown-email and wrong-password paths return the
// uniform [ErrInvalidCredentials] sentinel — callers must not be able to
// distinguish them.
//
// Order of operations:
//  1. Normalise email; fetch raw DB row (includes PasswordHash).
//  2. Unknown email ([sql.ErrNoRows]): call [Hasher.DummyVerify] for constant
//     KDF cost, then return [ErrInvalidCredentials].
//  3. Found: [Hasher.Verify] the stored PHC against the supplied password.
//     Mismatch → [ErrInvalidCredentials].
//  4. Match: if [Hasher.NeedsRehash] is true, compute a fresh hash and call
//     [Service.UpdatePasswordHash].  A rehash failure must NOT fail the login —
//     the original credentials were correct; we simply skip the upgrade.
//  5. Return [userFromRow](row), nil.
func (s *Service) Authenticate(ctx context.Context, email, password string) (User, error) {
	row, err := s.userRowByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Constant-time work so timing cannot reveal whether the email exists.
			_, _ = s.hasher.DummyVerify(password)
			return User{}, ErrInvalidCredentials
		}
		return User{}, fmt.Errorf("auth: authenticate lookup: %w", err)
	}

	ok, err := s.hasher.Verify(row.PasswordHash, password)
	if err != nil {
		// Malformed PHC stored in the DB — treat as failed auth but log via error
		// wrapping so infrastructure failures are distinguishable in metrics.
		return User{}, fmt.Errorf("auth: verify password hash: %w", err)
	}
	if !ok {
		return User{}, ErrInvalidCredentials
	}

	// Opportunistic rehash: upgrade weak stored params to current params on
	// login.  A failure must not fail the login — skip silently.
	if s.hasher.NeedsRehash(row.PasswordHash) {
		if newPHC, rehashErr := s.hasher.Hash(password); rehashErr == nil {
			_ = s.UpdatePasswordHash(ctx, row.ID, newPHC)
		}
	}

	return userFromRow(row), nil
}

// ── constraint detection ──────────────────────────────────────────────────────

// isUniqueConstraintError reports whether err is a SQLite UNIQUE constraint
// violation from the go-sqlcipher driver.  It uses [errors.As] against the
// driver's [sqlite3.Error] type to check the extended error code — no string
// matching.
func isUniqueConstraintError(err error) bool {
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique
	}
	return false
}
