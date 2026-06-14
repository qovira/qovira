-- Queries for the pending_confirmations table.
-- Every SELECT/UPDATE includes a user_id predicate so the row always
-- belongs to the bound Scope. Parameters use sqlc named params (@name).

-- name: InsertPendingConfirmation :one
INSERT INTO pending_confirmations (id, conversation_id, message_id, user_id, tool_name, args, risk, status, expires_at)
VALUES (@id, @conversation_id, @message_id, @user_id, @tool_name, @args, @risk, @status, @expires_at)
RETURNING id, conversation_id, message_id, user_id, tool_name, args, risk, status, created_at, expires_at;

-- name: GetPendingConfirmation :one
SELECT id, conversation_id, message_id, user_id, tool_name, args, risk, status, created_at, expires_at
FROM pending_confirmations
WHERE id = @id
  AND user_id = @user_id;

-- name: UpdatePendingConfirmationStatus :execrows
UPDATE pending_confirmations
SET status = @status
WHERE id = @id
  AND user_id = @user_id
  AND status = 'pending';

-- name: UpdatePendingConfirmationStatusIfCurrent :execrows
UPDATE pending_confirmations
SET status = @status
WHERE id = @id
  AND user_id = @user_id
  AND status = 'pending'
  AND NOT (expires_at < @now);

-- name: ListPendingConfirmationsByConversation :many
SELECT id, conversation_id, message_id, user_id, tool_name, args, risk, status, created_at, expires_at
FROM pending_confirmations
WHERE conversation_id = @conversation_id
  AND user_id = @user_id
ORDER BY created_at, id;

-- name: MarkConfirmationExpired :execrows
-- Atomic CAS: transitions a pending row to expired only when still pending.
-- Used by both the lazy check (user-scoped, includes user_id) and the sweep
-- (system-scope, but calls this per-row with the row's user_id from ListLapsedConfirmations).
UPDATE pending_confirmations
SET status = 'expired'
WHERE id = @id
  AND user_id = @user_id
  AND status = 'pending';

-- name: ListLapsedConfirmations :many
-- scopeguard:allow-unscoped: SYSTEM-HOUSEKEEPING cross-user sweep. The scheduler
-- calls SweepExpiredConfirmations across all users by TTL cutoff, so no single
-- user_id predicate is applicable. Each returned row carries its own user_id so
-- the caller can scope per-row operations (abandon message, emit per-user event).
SELECT id, conversation_id, message_id, user_id, tool_name, args, risk, status, created_at, expires_at
FROM pending_confirmations
WHERE status = 'pending'
  AND expires_at < @now
ORDER BY expires_at, id;

-- name: CountNonExpiredConfirmationsByMessageID :one
-- Returns the count of pending_confirmations rows for a given assistant message
-- that are NOT in 'expired' status (i.e. 'pending', 'approved', or 'denied').
-- Used to gate MarkMessageAbandoned: the assistant message is only abandoned when
-- this count is zero (no siblings remain pending or were resolved by the user).
-- User-scoped: requires user_id to prevent cross-user information leakage.
SELECT count(*)
FROM pending_confirmations
WHERE message_id = @message_id
  AND user_id = @user_id
  AND status != 'expired';
