package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/qovira/qovira/internal/auth"
	"github.com/qovira/qovira/internal/config"
	"github.com/qovira/qovira/internal/store"
)

func newAdminResetPasswordCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reset-password <email>",
		Short: "Reset a user's password from the CLI",
		Long: `Reset the password for the account identified by <email>.

The new password is resolved in this order:
  1. --password <value>       plain-text value on the command line
  2. --password-file <path>   read from a file (trailing newline trimmed)
  3. interactive prompt       prompted twice (with confirmation) when neither
                              flag is set; input is not echoed

Opening the store requires the master key (QOVIRA_MASTER_KEY or
QOVIRA_MASTER_KEY_FILE). All existing sessions for the account are revoked
so the user must log in again with the new password.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			email := args[0]

			// ── open store (mirror migrate.go pattern) ───────────────────
			cfgPath, _ := cmd.Flags().GetString("config")
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return fmt.Errorf("admin reset-password: load config: %w", err)
			}

			s, err := store.Open(store.Config{
				Path: store.DBPath(cfg.DataDir),
				Key:  string(cfg.MasterKey),
			})
			if err != nil {
				return fmt.Errorf("admin reset-password: open store: %w", err)
			}
			defer func() { _ = s.Close() }()

			// ── wire auth components ─────────────────────────────────────
			hasher := auth.NewHasher(auth.DefaultParams)
			// Policy is passed to NewService for future callers that go through the service; on this command path
			// policy is enforced explicitly in execResetPassword before hashing, so the service field is unused here.
			svc := auth.NewService(s, hasher, auth.DefaultPolicy)
			sessions := auth.NewSessions(s, auth.DefaultSessionConfig)

			// ── resolve new password ─────────────────────────────────────
			newPwd, err := resolvePassword(cmd)
			if err != nil {
				return fmt.Errorf("admin reset-password: %w", err)
			}

			// ── orchestrate ──────────────────────────────────────────────
			if err = execResetPassword(cmd, svc, sessions, hasher, email, newPwd); err != nil {
				return err
			}

			fmt.Fprintln(cmd.OutOrStdout(), "Password reset successfully.")
			return nil
		},
	}

	cmd.Flags().String("password", "", "new password (plain text)")
	cmd.Flags().String("password-file", "", "path to file containing the new password (trailing newline trimmed)")

	return cmd
}

// resolvePassword reads the new password from --password, --password-file, or an interactive double-prompt, in that
// order of precedence. Setting both --password and --password-file is an error, mirroring the config _FILE rule.
func resolvePassword(cmd *cobra.Command) (string, error) {
	pwFlag, _ := cmd.Flags().GetString("password")
	pwFileFlag, _ := cmd.Flags().GetString("password-file")

	switch {
	case pwFlag != "" && pwFileFlag != "":
		return "", errors.New("--password and --password-file are both set; use exactly one")

	case pwFileFlag != "":
		return readPasswordFile(pwFileFlag)

	case pwFlag != "":
		return pwFlag, nil

	default:
		return promptPassword()
	}
}

// readPasswordFile reads the file at path, trims trailing '\n' and '\r' characters (as produced by shell echo or
// Docker secrets), and returns the result. Mirrors the exact trim behaviour of the unexported readSecretFile in
// internal/config.
func readPasswordFile(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-provided path; intentional
	if err != nil {
		return "", fmt.Errorf("read password file: %w", err)
	}
	return strings.TrimRight(string(data), "\n\r"), nil
}

// promptPassword reads the new password interactively (no echo) and confirms it by prompting a second time. Returns an
// error when the two entries do not match.
func promptPassword() (string, error) {
	fmt.Fprint(os.Stderr, "New password: ")
	first, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr) // newline after the hidden input
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}

	fmt.Fprint(os.Stderr, "Confirm new password: ")
	second, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read password confirmation: %w", err)
	}

	if string(first) != string(second) {
		return "", errors.New("passwords do not match")
	}
	return string(first), nil
}

// execResetPassword orchestrates the lookup → policy check → hash → store → revoke sequence. Keeping it separate from
// the cobra RunE makes it testable without a real TTY.
func execResetPassword(
	cmd *cobra.Command,
	svc *auth.Service,
	sessions *auth.Sessions,
	hasher *auth.Hasher,
	email, newPwd string,
) error {
	ctx := cmd.Context()

	// Resolve user — fail fast when the email is unknown.
	user, err := svc.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return fmt.Errorf("user %q not found", email)
		}
		return fmt.Errorf("look up user: %w", err)
	}

	// Enforce password policy before touching the DB. The policy field on the Service is not used on this path — we
	// validate explicitly so the error message is user-facing and the DB is never touched on a policy violation.
	if err = auth.DefaultPolicy.ValidatePassword(newPwd); err != nil {
		return fmt.Errorf("password policy: %w", err)
	}

	// Hash the new password.
	phc, err := hasher.Hash(newPwd)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	// Persist the new hash.
	if err = svc.UpdatePasswordHash(ctx, user.ID, phc); err != nil {
		return fmt.Errorf("update password: %w", err)
	}

	// Revoke all sessions so the user must log in again.
	if err = sessions.DeleteAllForUser(ctx, user.ID); err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}

	return nil
}
