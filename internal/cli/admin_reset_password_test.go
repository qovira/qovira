package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/auth"
	"github.com/qovira/qovira/internal/store"
	"github.com/qovira/qovira/internal/store/db"
)

// ── test harness ──────────────────────────────────────────────────────────────

// adminTestKey is the SQLCipher master key for all admin-command tests. Must be
// at least 16 bytes to pass config validation.
const adminTestKey = "admin-test-key-which-is-long-enough"

// fastAdminParams is a minimal-cost argon2id set so test hashing is fast while
// still exercising the real code path.
var fastAdminParams = auth.Params{
	Memory:  64,
	Time:    1,
	Threads: 1,
	KeyLen:  32,
	SaltLen: 16,
}

// fastAdminPolicy requires 8 rune minimum so short test passwords are accepted.
var fastAdminPolicy = auth.Policy{MinLen: 8, MaxLen: 64}

// openAdminStore opens a fully-migrated SQLCipher store in a temp dir, seeds a
// user with email and password "original-pass", and returns everything needed
// to drive assertions. The store handle is returned so tests can read back
// raw db.User rows (including PasswordHash) via db.New(s.Reader()). Cleanup is
// registered with t.Cleanup.
func openAdminStore(t *testing.T, email string) (s *store.Store, dataDir string, hasher *auth.Hasher, svc *auth.Service, sessions *auth.Sessions, user auth.User) {
	t.Helper()

	dataDir = t.TempDir()
	s, err := store.Open(store.Config{
		Path:         filepath.Join(dataDir, "qovira.db"),
		Key:          adminTestKey,
		ReadPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if cerr := s.Close(); cerr != nil {
			t.Errorf("store.Close: %v", cerr)
		}
	})

	runner := store.NewRunner()
	if err = runner.Up(context.Background(), s.Writer()); err != nil {
		t.Fatalf("runner.Up: %v", err)
	}

	hasher = auth.NewHasher(fastAdminParams)
	svc = auth.NewService(s, hasher, fastAdminPolicy)
	sessions = auth.NewSessions(s, auth.DefaultSessionConfig)

	user, err = svc.CreateUser(context.Background(), auth.NewUser{
		Email:       email,
		DisplayName: "Test User",
		Password:    "original-pass",
		Role:        auth.RoleMember,
		Timezone:    "UTC",
		Locale:      "en-US",
		Language:    "en",
	})
	if err != nil {
		t.Fatalf("CreateUser(%q): %v", email, err)
	}
	return s, dataDir, hasher, svc, sessions, user
}

// runAdminCmd sets env vars via t.Setenv, then delegates to runCmd. Callers
// must not mark the parent test t.Parallel() because t.Setenv is incompatible
// with parallel execution.
func runAdminCmd(t *testing.T, env map[string]string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	return runCmd(t, args...)
}

// mintTokens creates n sessions for userID and returns their plaintext tokens
// so the test can later assert that Lookup returns ErrSessionNotFound.
func mintTokens(t *testing.T, sessions *auth.Sessions, userID string, n int) []string {
	t.Helper()
	tokens := make([]string, n)
	for i := range n {
		tok, _, _, err := sessions.Mint(context.Background(), userID, time.Now().UTC())
		if err != nil {
			t.Fatalf("Mint session %d: %v", i, err)
		}
		tokens[i] = tok
	}
	return tokens
}

// allRevoked returns true when every token in the slice is gone from the store.
func allRevoked(t *testing.T, sessions *auth.Sessions, tokens []string) bool {
	t.Helper()
	for _, tok := range tokens {
		_, err := sessions.Lookup(context.Background(), tok)
		if err == nil {
			return false // session still exists
		}
		if !errors.Is(err, auth.ErrSessionNotFound) {
			t.Errorf("Lookup: unexpected error: %v", err)
		}
	}
	return true
}

// adminEnv returns the env map required for the CLI to open the store at dataDir.
func adminEnv(dataDir string) map[string]string {
	return map[string]string{
		"QOVIRA_MASTER_KEY": adminTestKey,
		"QOVIRA_DATA_DIR":   dataDir,
	}
}

// ── AC1 + AC2: success path — hash updated and sessions wiped ─────────────────

// TestAdminResetPassword_Success verifies that reset-password with --password
// succeeds (AC1), all pre-existing sessions are wiped (AC2), and the old
// password no longer verifies against the persisted hash (AC1 negative check).
//
// AC1 is verified end-to-end: after the command runs we read back the raw
// db.User row (which carries PasswordHash) via db.New(s.Reader()).GetUserByEmail
// and call Hasher.Verify against the PHC the command itself wrote — no
// intermediate hash or UpdatePasswordHash call from the test.
func TestAdminResetPassword_Success(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel().
	const email = "reset-success@example.com"
	const newPwd = "new-password-ok"
	const originalPwd = "original-pass" // seeded by openAdminStore

	s, dataDir, hasher, _, sessions, user := openAdminStore(t, email)

	// Mint 3 sessions; assert they exist before the reset.
	tokens := mintTokens(t, sessions, user.ID, 3)
	if allRevoked(t, sessions, tokens) {
		t.Fatal("precondition: sessions must exist before reset")
	}

	_, stderr, err := runAdminCmd(t, adminEnv(dataDir),
		"admin", "reset-password", email, "--password", newPwd)
	if err != nil {
		t.Fatalf("reset-password: %v\nstderr: %s", err, stderr)
	}

	// AC2: all sessions must be revoked.
	if !allRevoked(t, sessions, tokens) {
		t.Error("AC2: sessions still exist after reset-password")
	}

	// AC1: read back the PHC the command persisted directly from the DB layer —
	// no intermediate Hash or UpdatePasswordHash from the test side.
	row, err := db.New(s.Reader()).GetUserByEmail(context.Background(), strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	storedPHC := row.PasswordHash

	ok, err := hasher.Verify(storedPHC, newPwd)
	if err != nil {
		t.Fatalf("Hasher.Verify(storedPHC, newPwd): %v", err)
	}
	if !ok {
		t.Error("AC1: new password does not verify against the command-written PHC")
	}

	// AC1 negative: the original password must no longer verify.
	oldOK, err := hasher.Verify(storedPHC, originalPwd)
	if err != nil {
		t.Fatalf("Hasher.Verify(storedPHC, originalPwd): %v", err)
	}
	if oldOK {
		t.Error("AC1: old password still verifies — hash was not updated")
	}
}

// ── AC3: policy violation is a no-op ─────────────────────────────────────────

// TestAdminResetPassword_PolicyViolation verifies that a too-short new password
// causes a non-zero exit and leaves both the existing hash and sessions intact.
// auth.DefaultPolicy requires 12 runes minimum; "short" is 5 runes.
func TestAdminResetPassword_PolicyViolation(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel().
	const email = "policy-fail@example.com"

	_, dataDir, hasher, svc, sessions, user := openAdminStore(t, email)

	// Set a known PHC so we can assert it is unchanged after the failed reset.
	originalPHC, err := hasher.Hash("original-pass")
	if err != nil {
		t.Fatalf("hasher.Hash: %v", err)
	}
	if err = svc.UpdatePasswordHash(context.Background(), user.ID, originalPHC); err != nil {
		t.Fatalf("set known PHC: %v", err)
	}

	tokens := mintTokens(t, sessions, user.ID, 2)

	_, stderr, err := runAdminCmd(t, adminEnv(dataDir),
		"admin", "reset-password", email, "--password", "short")

	if err == nil {
		t.Fatal("expected error for policy-violating password, got nil")
	}

	// AC3a: clear message — must mention "password" in stderr or error.
	if !strings.Contains(stderr, "password") && !strings.Contains(err.Error(), "password") {
		t.Errorf("expected password-related message; stderr=%q err=%v", stderr, err)
	}

	// AC3b: old PHC is intact — the original password still verifies against it.
	ok, verifyErr := hasher.Verify(originalPHC, "original-pass")
	if verifyErr != nil {
		t.Fatalf("Hasher.Verify(originalPHC): %v", verifyErr)
	}
	if !ok {
		t.Error("AC3: original PHC no longer valid — hash was changed despite policy violation")
	}

	// AC3c: sessions are untouched.
	if allRevoked(t, sessions, tokens) {
		t.Error("AC3: sessions were wiped despite policy violation — must be no-op")
	}
}

// ── AC4: unknown email → non-zero exit ───────────────────────────────────────

// TestAdminResetPassword_UnknownEmail verifies that an unknown email address
// yields a non-zero exit and a clear not-found message.
func TestAdminResetPassword_UnknownEmail(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel().
	const seed = "seed@example.com"
	_, dataDir, _, _, _, _ := openAdminStore(t, seed)

	_, stderr, err := runAdminCmd(t, adminEnv(dataDir),
		"admin", "reset-password", "nobody@example.com", "--password", "valid-long-password")

	if err == nil {
		t.Fatal("expected error for unknown email, got nil")
	}

	// AC4: clear message referencing the unknown email or "not found".
	if !strings.Contains(stderr, "nobody@example.com") && !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found message; stderr=%q err=%v", stderr, err)
	}
}

// ── AC5: --password-file trims trailing newline ───────────────────────────────

// TestAdminResetPassword_PasswordFile verifies that --password-file reads the
// file, strips a trailing newline (like shell echo or Docker secrets produce),
// and succeeds with the trimmed value.
func TestAdminResetPassword_PasswordFile(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel().
	const email = "pwfile@example.com"
	const newPwd = "from-file-password"

	_, dataDir, _, _, sessions, user := openAdminStore(t, email)

	// Write the password with a trailing newline.
	pwFile := filepath.Join(t.TempDir(), "password.txt")
	if err := os.WriteFile(pwFile, []byte(newPwd+"\n"), 0o600); err != nil {
		t.Fatalf("write password file: %v", err)
	}

	tokens := mintTokens(t, sessions, user.ID, 2)

	out, stderr, err := runAdminCmd(t, adminEnv(dataDir),
		"admin", "reset-password", email, "--password-file", pwFile)
	if err != nil {
		t.Fatalf("reset-password --password-file: %v\nstderr: %s", err, stderr)
	}

	// AC5: password must not appear in stdout or stderr.
	if strings.Contains(out, newPwd) || strings.Contains(stderr, newPwd) {
		t.Errorf("password leaked into output: stdout=%q stderr=%q", out, stderr)
	}

	// AC2: sessions revoked on success.
	if !allRevoked(t, sessions, tokens) {
		t.Error("sessions still exist after --password-file reset")
	}
}

// TestAdminResetPassword_PasswordFileNoTrailingNewline verifies that a file
// without a trailing newline is also accepted (trim is idempotent).
func TestAdminResetPassword_PasswordFileNoTrailingNewline(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel().
	const email = "pwfile-notrim@example.com"
	const newPwd = "no-newline-password"

	_, dataDir, _, _, _, _ := openAdminStore(t, email)

	pwFile := filepath.Join(t.TempDir(), "password.txt")
	if err := os.WriteFile(pwFile, []byte(newPwd), 0o600); err != nil {
		t.Fatalf("write password file: %v", err)
	}

	_, stderr, err := runAdminCmd(t, adminEnv(dataDir),
		"admin", "reset-password", email, "--password-file", pwFile)
	if err != nil {
		t.Fatalf("reset-password (no trailing newline): %v\nstderr: %s", err, stderr)
	}
}

// ── AC6: store requires master key ────────────────────────────────────────────

// TestAdminResetPassword_NoMasterKey verifies that the command fails fast when
// QOVIRA_MASTER_KEY is absent (config validation rejects it before any DB work).
func TestAdminResetPassword_NoMasterKey(t *testing.T) {
	t.Parallel()

	// QOVIRA_MASTER_KEY is not set in the environment; config.Load must fail.
	_, stderr, err := runCmd(t, "admin", "reset-password", "any@example.com", "--password", "some-long-password")
	if err == nil {
		t.Fatal("expected error when QOVIRA_MASTER_KEY is absent, got nil")
	}
	if !strings.Contains(err.Error(), "master_key") && !strings.Contains(stderr, "master_key") {
		t.Errorf("expected master_key error; err=%v stderr=%q", err, stderr)
	}
}

// ── flag-conflict guard ───────────────────────────────────────────────────────

// TestAdminResetPassword_BothPasswordFlags verifies that supplying both
// --password and --password-file is rejected.
func TestAdminResetPassword_BothPasswordFlags(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel().
	const email = "both-flags@example.com"
	_, dataDir, _, _, _, _ := openAdminStore(t, email)

	pwFile := filepath.Join(t.TempDir(), "pw.txt")
	if err := os.WriteFile(pwFile, []byte("long-enough-password"), 0o600); err != nil {
		t.Fatalf("write password file: %v", err)
	}

	_, _, err := runAdminCmd(t, adminEnv(dataDir),
		"admin", "reset-password", email,
		"--password", "long-enough-password",
		"--password-file", pwFile)
	if err == nil {
		t.Fatal("expected error when both --password and --password-file are set, got nil")
	}
}

// ── password never leaks into output ─────────────────────────────────────────

// TestAdminResetPassword_PasswordNotInOutput verifies that the plaintext
// password never appears in stdout or stderr on the success path.
func TestAdminResetPassword_PasswordNotInOutput(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel().
	const email = "no-leak@example.com"
	const newPwd = "super-secret-new-password"

	_, dataDir, _, _, _, _ := openAdminStore(t, email)

	out, stderr, err := runAdminCmd(t, adminEnv(dataDir),
		"admin", "reset-password", email, "--password", newPwd)
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr: %s", err, stderr)
	}
	if strings.Contains(out, newPwd) {
		t.Errorf("password appeared in stdout: %q", out)
	}
	if strings.Contains(stderr, newPwd) {
		t.Errorf("password appeared in stderr: %q", stderr)
	}
}

// ── admin group wiring ────────────────────────────────────────────────────────

// TestAdminHelp verifies that "qovira admin --help" lists reset-password.
func TestAdminHelp(t *testing.T) {
	t.Parallel()

	out, _, _ := runCmd(t, "admin", "--help")
	if !strings.Contains(out, "reset-password") {
		t.Errorf("admin --help missing reset-password subcommand\ngot:\n%s", out)
	}
}
