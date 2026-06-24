package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/qovira/qovira/internal/id"
	"github.com/qovira/qovira/internal/store"
	"github.com/qovira/qovira/internal/store/db"
)

// ── Sentinel errors ───────────────────────────────────────────────────────────

// ErrSessionNotFound is returned by [Sessions.Lookup] when no session row
// matches the provided token.  Use [errors.Is] to check.
var ErrSessionNotFound = errors.New("session not found")

// ── SessionConfig ─────────────────────────────────────────────────────────────

// SessionConfig holds the TTL and bump-throttle parameters for the session
// layer.  All three fields are required; use [DefaultSessionConfig] for sane
// production defaults.
//
// Validity is always computed from the anchor timestamps stored in the DB, so
// changing these values affects all live sessions immediately with no migration.
type SessionConfig struct {
	// IdleTTL is the sliding idle timeout: a session expires if it has not been
	// used within this window.  Production default: 7 days.
	IdleTTL time.Duration

	// AbsoluteTTL is the hard cap on session lifetime regardless of activity.
	// Production default: 30 days.
	AbsoluteTTL time.Duration

	// BumpInterval is the minimum time between last_used_at writes.  A [Sessions.Bump]
	// call within this interval is silently skipped (returns false).  Setting this
	// to a small fraction of IdleTTL (e.g. 15 min vs 7 days) amortises write load
	// across frequent requests.  Production default: 15 minutes.
	BumpInterval time.Duration
}

// DefaultSessionConfig is the production session configuration:
//   - IdleTTL:      7 days
//   - AbsoluteTTL:  30 days
//   - BumpInterval: 15 minutes
var DefaultSessionConfig = SessionConfig{
	IdleTTL:      7 * 24 * time.Hour,
	AbsoluteTTL:  30 * 24 * time.Hour,
	BumpInterval: 15 * time.Minute,
}

// ── Session ───────────────────────────────────────────────────────────────────

// Session is the safe session record returned to callers.  It deliberately omits
// the token plaintext and the token_hash so neither can leak into logs or API
// responses.
type Session struct {
	// ID is the ULID primary key of the session row.
	ID string

	// UserID is the ULID of the owning user.
	UserID string

	// CreatedAt is when the session was minted.  Anchors the absolute TTL cap.
	CreatedAt time.Time

	// LastUsedAt is when the session was last seen (bumped).  Anchors the
	// sliding idle window.
	LastUsedAt time.Time
}

// sessionFromRow converts a generated [db.Session] into the public [Session]
// type, parsing the RFC 3339 timestamp strings into [time.Time] values.
// It is the only place this conversion happens so that token_hash omission is
// enforced structurally.
//
// Timestamps are truncated to second precision on the way in because the DB
// stores RFC 3339 strings (no sub-second component); callers that compare
// Session timestamps against time.Now() must truncate their own values too, or
// accept that DB-roundtripped times have second precision.
func sessionFromRow(row db.Session) (Session, error) {
	createdAt, lastUsedAt, err := parseSessionTimes(row.CreatedAt, row.LastUsedAt)
	if err != nil {
		return Session{}, err
	}
	return Session{
		ID:         row.ID,
		UserID:     row.UserID,
		CreatedAt:  createdAt,
		LastUsedAt: lastUsedAt,
	}, nil
}

// sessionFromJoinRow converts a [db.GetSessionWithUserByTokenHashRow] (the
// result of the joined session+user query) into a [Session] for use by Resolve.
// Only the timestamp fields are needed; the caller reads UserID and Role
// directly from the row.
func sessionFromJoinRow(row db.GetSessionWithUserByTokenHashRow) (Session, error) {
	createdAt, lastUsedAt, err := parseSessionTimes(row.CreatedAt, row.LastUsedAt)
	if err != nil {
		return Session{}, err
	}
	return Session{
		ID:         row.ID,
		UserID:     row.UserID,
		CreatedAt:  createdAt,
		LastUsedAt: lastUsedAt,
	}, nil
}

// parseSessionTimes parses the created_at and last_used_at RFC 3339 strings a
// session row carries and returns them normalized to UTC. It is the single
// parse site both row-conversion helpers delegate to so the timestamp contract
// (RFC 3339, UTC, second precision) stays in lockstep across query shapes.
//
// UTC enforcement: both timestamps must carry a zero offset (i.e. the "Z"
// suffix, or equivalently "+00:00"). Mint and Bump always write canonical UTC
// strings, so a non-zero offset indicates a row written by external tooling or
// a pre-guard code path. Rejecting such rows here ensures that the in-memory
// Valid/ExpiresAt computation and the raw-string lexicographic comparison in
// PurgeExpired (SQL) always agree — a "+02:00" row would otherwise Resolve
// correctly yet sort wrong in the purge query.
func parseSessionTimes(createdAt, lastUsedAt string) (time.Time, time.Time, error) {
	created, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("auth: parse session created_at %q: %w", createdAt, err)
	}
	if _, offset := created.Zone(); offset != 0 {
		return time.Time{}, time.Time{}, fmt.Errorf("auth: session created_at %q has non-UTC offset; only canonical UTC (Z) timestamps are accepted", createdAt)
	}
	lastUsed, err := time.Parse(time.RFC3339, lastUsedAt)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("auth: parse session last_used_at %q: %w", lastUsedAt, err)
	}
	if _, offset := lastUsed.Zone(); offset != 0 {
		return time.Time{}, time.Time{}, fmt.Errorf("auth: session last_used_at %q has non-UTC offset; only canonical UTC (Z) timestamps are accepted", lastUsedAt)
	}
	return created.UTC(), lastUsed.UTC(), nil
}

// ── Sessions ──────────────────────────────────────────────────────────────────

// Sessions provides session-lifecycle operations for the Qovira identity layer:
// minting, lookup, validity checking, throttled bumping, revocation, and purging
// of expired rows.
//
// Construct it via [NewSessions]; the zero value is not valid.
//
// All time-dependent methods accept an explicit now time.Time parameter so
// callers control the clock.  Production callers pass time.Now().UTC(); tests
// inject a synthetic value to avoid real sleeps and make TTL boundary tests
// deterministic.
//
// The session store never logs anything: bump failures and expiry decisions are
// returned to the caller (middleware) to handle non-fatally.
type Sessions struct {
	s      *store.Store
	readQ  *db.Queries
	writeQ *db.Queries
	cfg    SessionConfig
}

// NewSessions constructs a Sessions backed by the provided store and config.
// Reads go through the read pool; writes go through the write pool.
func NewSessions(s *store.Store, cfg SessionConfig) *Sessions {
	return &Sessions{
		s:      s,
		readQ:  db.New(s.Reader()),
		writeQ: db.New(s.Writer()),
		cfg:    cfg,
	}
}

// Store returns the underlying [store.Store].  Exposed so test helpers that need
// the store reference (e.g. to open a second Sessions with a different config)
// can obtain it without a separate parameter.
func (ss *Sessions) Store() *store.Store {
	return ss.s
}

// ── Mint ──────────────────────────────────────────────────────────────────────

// Mint creates a new session for userID and returns the plaintext bearer token,
// the stored [Session] record (without the token or token_hash), and the computed
// expiry time.
//
// Token format: "qov_" + base64.RawURLEncoding(32 random bytes) — 47 characters
// total, 256 bits of entropy.  Only sha256(token) is stored in the DB.
//
// now sets both created_at and last_used_at on the new row.  It is truncated to
// second precision before storage (RFC 3339 carries no sub-second component);
// the returned Session carries the same truncated value so callers can compare
// timestamps against DB-roundtripped values without surprises.
func (ss *Sessions) Mint(ctx context.Context, userID string, now time.Time) (token string, sess Session, expiresAt time.Time, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", Session{}, time.Time{}, fmt.Errorf("auth: generate session token: %w", err)
	}
	token = "qov_" + base64.RawURLEncoding.EncodeToString(raw)

	hashArr := sha256.Sum256([]byte(token))
	tokenHash := hashArr[:]

	// Truncate to second precision: RFC 3339 has no sub-second component, so any
	// nanoseconds would be silently dropped on storage. Returning the truncated
	// value makes Session timestamps consistently comparable to DB-roundtripped
	// values without callers needing to know the storage precision.
	nowSec := now.UTC().Truncate(time.Second)
	nowStr := nowSec.Format(time.RFC3339)
	sessID := id.New()

	if err = ss.writeQ.CreateSession(ctx, db.CreateSessionParams{
		ID:         sessID,
		UserID:     userID,
		TokenHash:  tokenHash,
		CreatedAt:  nowStr,
		LastUsedAt: nowStr,
	}); err != nil {
		return "", Session{}, time.Time{}, fmt.Errorf("auth: create session: %w", err)
	}

	sess = Session{
		ID:         sessID,
		UserID:     userID,
		CreatedAt:  nowSec,
		LastUsedAt: nowSec,
	}
	expiresAt = ss.ExpiresAt(sess)
	return token, sess, expiresAt, nil
}

// ── Lookup ────────────────────────────────────────────────────────────────────

// Lookup retrieves the session associated with the plaintext bearer token.  It
// hashes the token before querying so the plaintext never reaches the DB layer.
//
// Returns [ErrSessionNotFound] when no row matches.  Lookup does NOT check
// expiry; the caller is responsible for calling [Sessions.Valid] and handling
// the result (the middleware slice owns that logic).
func (ss *Sessions) Lookup(ctx context.Context, token string) (Session, error) {
	hashArr := sha256.Sum256([]byte(token))
	row, err := ss.readQ.GetSessionByTokenHash(ctx, hashArr[:])
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, ErrSessionNotFound
		}
		return Session{}, fmt.Errorf("auth: lookup session: %w", err)
	}
	sess, err := sessionFromRow(row)
	if err != nil {
		return Session{}, err
	}
	return sess, nil
}

// ── Validity helpers ──────────────────────────────────────────────────────────

// Valid reports whether sess is still valid at now.  A session is valid iff now
// is before BOTH the idle deadline (lastUsedAt + IdleTTL) and the absolute cap
// (createdAt + AbsoluteTTL).
//
// This is a pure function of the session anchors and the config; no DB access
// is performed.
func (ss *Sessions) Valid(sess Session, now time.Time) bool {
	idleDeadline := sess.LastUsedAt.Add(ss.cfg.IdleTTL)
	absDeadline := sess.CreatedAt.Add(ss.cfg.AbsoluteTTL)
	return now.Before(idleDeadline) && now.Before(absDeadline)
}

// ExpiresAt returns the earlier of the idle deadline (lastUsedAt + IdleTTL) and
// the absolute cap (createdAt + AbsoluteTTL).
//
// This is a pure function; no DB access is performed.
func (ss *Sessions) ExpiresAt(sess Session) time.Time {
	idleDeadline := sess.LastUsedAt.Add(ss.cfg.IdleTTL)
	absDeadline := sess.CreatedAt.Add(ss.cfg.AbsoluteTTL)
	if idleDeadline.Before(absDeadline) {
		return idleDeadline
	}
	return absDeadline
}

// ── Bump ──────────────────────────────────────────────────────────────────────

// Bump updates last_used_at to now for the session identified by sess.ID and
// sess.UserID, but only when now.Sub(sess.LastUsedAt) >= BumpInterval.  If the
// interval has not elapsed, Bump is a no-op and returns (false, nil).
//
// A bump that matches zero rows (session deleted between the throttle check and
// the UPDATE) returns (false, nil), not an error.
//
// Design note: returning the error lets the middleware log it non-fatally without
// importing a logger here.
func (ss *Sessions) Bump(ctx context.Context, sess Session, now time.Time) (bumped bool, err error) {
	if now.Sub(sess.LastUsedAt) < ss.cfg.BumpInterval {
		return false, nil
	}
	// Truncate to second precision to match storage format (RFC 3339).
	nowStr := now.UTC().Truncate(time.Second).Format(time.RFC3339)
	n, err := ss.writeQ.BumpSessionLastUsedByID(ctx, db.BumpSessionLastUsedByIDParams{
		LastUsedAt: nowStr,
		ID:         sess.ID,
		UserID:     sess.UserID,
	})
	if err != nil {
		return false, fmt.Errorf("auth: bump session: %w", err)
	}
	return n > 0, nil
}

// ── Revocation ────────────────────────────────────────────────────────────────

// DeleteByToken removes the session identified by the plaintext bearer token.
// It is used for single-session logout and best-effort delete-on-expiry.
// A no-op when the token does not exist.
func (ss *Sessions) DeleteByToken(ctx context.Context, token string) error {
	hashArr := sha256.Sum256([]byte(token))
	if _, err := ss.writeQ.DeleteSessionByTokenHash(ctx, hashArr[:]); err != nil {
		return fmt.Errorf("auth: delete session by token: %w", err)
	}
	return nil
}

// DeleteAllForUser removes every session owned by userID.  Used for
// logout-everywhere (e.g. on password change when kill-others semantics are
// required for the current session too).
func (ss *Sessions) DeleteAllForUser(ctx context.Context, userID string) error {
	if _, err := ss.writeQ.DeleteSessionsForUser(ctx, userID); err != nil {
		return fmt.Errorf("auth: delete sessions for user: %w", err)
	}
	return nil
}

// DeleteAllOtherForUser removes every session owned by userID except the one
// with keepSessionID.  Used when the caller wants to invalidate all other
// devices/sessions while keeping the current one alive (e.g. password-change
// kill-others).
func (ss *Sessions) DeleteAllOtherForUser(ctx context.Context, userID, keepSessionID string) error {
	if _, err := ss.writeQ.DeleteOtherSessionsForUser(ctx, db.DeleteOtherSessionsForUserParams{
		UserID: userID,
		KeepID: keepSessionID,
	}); err != nil {
		return fmt.Errorf("auth: delete other sessions for user: %w", err)
	}
	return nil
}

// ── Resolve ───────────────────────────────────────────────────────────────────

// Resolve validates the plaintext bearer token and, on success, returns the
// authenticated [store.Principal].  It is the single-entry point for the auth
// middleware: it hashes the token, runs the joined session+user query on the
// read pool, applies the dual-timeout check, and issues a throttled bump.
//
// Error semantics:
//   - [ErrSessionNotFound] when the token is unknown or the session has expired
//     (callers should treat both as "unauthenticated").
//   - Any other non-nil error is an infrastructure failure.
//
// On expiry Resolve performs a best-effort [DeleteByToken] (ignoring its error)
// so the row is cleaned up without blocking the response.
//
// On success Resolve calls [Sessions.Bump] to slide the idle window; a bump
// failure is silently ignored (logged by the caller if desired) and must never
// fail the resolution.
func (ss *Sessions) Resolve(ctx context.Context, token string, now time.Time) (store.Principal, error) {
	hashArr := sha256.Sum256([]byte(token))
	row, err := ss.readQ.GetSessionWithUserByTokenHash(ctx, hashArr[:])
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Principal{}, ErrSessionNotFound
		}
		return store.Principal{}, fmt.Errorf("auth: resolve session: %w", err)
	}

	// Reconstruct a Session so we can reuse the existing Valid/Bump logic rather
	// than duplicating the timeout and throttle calculations here.
	sess, err := sessionFromJoinRow(row)
	if err != nil {
		return store.Principal{}, err
	}

	if !ss.Valid(sess, now) {
		// Best-effort cleanup: ignore the error — it must not surface to the caller.
		_ = ss.DeleteByToken(ctx, token)
		return store.Principal{}, ErrSessionNotFound
	}

	// Best-effort throttled bump: a failure must not fail the resolution.
	_, _ = ss.Bump(ctx, sess, now)

	return store.Principal{
		UserID: row.UserID,
		Role:   row.Role,
	}, nil
}

// ── PurgeExpired ──────────────────────────────────────────────────────────────

// PurgeExpired deletes all sessions that are expired at now — either because
// last_used_at is older than IdleTTL or because created_at is older than
// AbsoluteTTL.  Returns the number of deleted rows.
//
// This is designed to be called periodically by a background scheduler; it
// operates across all users (no user_id predicate) and is safe to call
// concurrently.
func (ss *Sessions) PurgeExpired(ctx context.Context, now time.Time) (int64, error) {
	idleCutoff := now.UTC().Add(-ss.cfg.IdleTTL).Format(time.RFC3339)
	absCutoff := now.UTC().Add(-ss.cfg.AbsoluteTTL).Format(time.RFC3339)

	n, err := ss.writeQ.PurgeExpiredSessions(ctx, db.PurgeExpiredSessionsParams{
		IdleCutoff:     idleCutoff,
		AbsoluteCutoff: absCutoff,
	})
	if err != nil {
		return 0, fmt.Errorf("auth: purge expired sessions: %w", err)
	}
	return n, nil
}
