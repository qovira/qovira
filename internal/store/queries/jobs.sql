-- Queries for the jobs table (durable scheduler queue).
-- The scheduler accesses jobs cross-user: the claim selects due jobs across ALL users and
-- resolves scope per-row. This is intentionally unscoped -- each query below carries an
-- explicit scopeguard annotation with the reason.
--
-- Parameters use sqlc named params (@name) per the house convention.
--
-- NOTE: Several operations use raw SQL via s.Writer() rather than sqlc queries, because
-- sqlc's SQLite ANTLR parser cannot model the required shapes:
--   1. INSERT ... ON CONFLICT(key) DO NOTHING: the upsert clause is not supported.
--   2. UPDATE ... WHERE id IN (SELECT ... LIMIT @batch) RETURNING ...: compound UPDATE.
-- These are executed as s.Writer().ExecContext / QueryContext in internal/scheduler.

-- name: GetJobIDByKey :one
-- scopeguard:allow-unscoped: cross-user lookup by idempotency key. The scheduler resolves
-- the existing job id when ON CONFLICT(key) DO NOTHING suppresses the insert.
SELECT id FROM jobs WHERE key = @key;

-- name: DeleteJob :exec
-- scopeguard:allow-unscoped: SYSTEM ENGINE -- the scheduler deletes the row after a handler
-- succeeds. The scheduler owns the row lifecycle; no user_id predicate is applicable.
DELETE FROM jobs WHERE id = @id;
