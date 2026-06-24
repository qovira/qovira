-- Queries for the sessions table.
-- sessions is per-user data (it has a user_id column and belongs to individual users), but some queries operate on a
-- token_hash key (the bearer capability itself) rather than a user_id predicate.  Those queries carry a per-query
-- scopeguard exemption with an explicit reason.  The bump query is keyed by id+user_id and needs no annotation.
-- INSERT is always scopeguard-exempt.
--
-- Parameters use sqlc named params (@name) per the house convention.

-- name: CreateSession :exec
INSERT INTO sessions (id, user_id, token_hash, created_at, last_used_at)
VALUES (@id, @user_id, @token_hash, @created_at, @last_used_at);

-- name: GetSessionByTokenHash :one
-- scopeguard:allow-unscoped: token_hash is the sha256 of a 256-bit bearer capability that itself authorizes
-- access; a session is resolved before any Principal exists, so no user_id predicate is possible or meaningful at
-- this lookup stage.
SELECT id, user_id, token_hash, created_at, last_used_at
FROM sessions
WHERE token_hash = @token_hash;

-- name: GetSessionWithUserByTokenHash :one
-- scopeguard:allow-unscoped: resolved before any Principal exists, keyed by the bearer token_hash capability; no
-- user_id predicate is possible at this pre-auth lookup stage. The JOIN to users retrieves the role in one read so
-- the middleware can construct a store.Principal without a second DB round-trip.
SELECT sessions.id, sessions.user_id, sessions.created_at, sessions.last_used_at, users.role
FROM sessions
JOIN users ON sessions.user_id = users.id
WHERE sessions.token_hash = @token_hash;

-- name: BumpSessionLastUsedByID :execrows
UPDATE sessions SET last_used_at = @last_used_at WHERE id = @id AND user_id = @user_id;

-- name: DeleteSessionByTokenHash :execrows
-- scopeguard:allow-unscoped: token_hash is the sha256 of a 256-bit bearer capability that itself authorizes
-- access; this path is used for single-session logout and best-effort delete-on-expiry, both keyed by the bearer
-- token before a user_id is available.
DELETE FROM sessions
WHERE token_hash = @token_hash;

-- name: DeleteSessionsForUser :execrows
DELETE FROM sessions
WHERE user_id = @user_id;

-- name: DeleteOtherSessionsForUser :execrows
DELETE FROM sessions
WHERE user_id = @user_id
  AND id != @keep_id;

-- name: PurgeExpiredSessions :execrows
-- scopeguard:allow-unscoped: system housekeeping; the scheduler purges across all users by TTL cutoffs (idle and
-- absolute), so there is no meaningful user context available and a user_id predicate would prevent cross-user
-- expiry from working.
DELETE FROM sessions
WHERE last_used_at < @idle_cutoff
   OR created_at < @absolute_cutoff;
