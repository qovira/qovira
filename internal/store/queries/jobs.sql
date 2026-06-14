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

-- name: GetJobStatus :one
-- scopeguard:allow-unscoped: SYSTEM ENGINE -- the scheduler reads the status of any job
-- regardless of owner to implement Cancel/Reschedule atomicity. Called inside a transaction
-- on the write pool to provide a consistent read-then-write with no TOCTOU window.
SELECT status FROM jobs WHERE id = @id;

-- name: RescheduleJob :execrows
-- scopeguard:allow-unscoped: SYSTEM ENGINE -- the scheduler moves run_at for any pending job.
-- Only updates rows with status='pending'; returns 0 rows affected when the job is not pending
-- (running or absent today; 'failed'/dead-letter is a later slice). The caller interprets a
-- non-pending status as ErrJobRunning or ErrJobNotFound
-- after reading the status inside the enclosing transaction.
UPDATE jobs SET run_at = @run_at, updated_at = @updated_at
WHERE id = @id AND status = 'pending';

-- name: RetryJob :execrows
-- scopeguard:allow-unscoped: SYSTEM ENGINE -- the scheduler re-arms a failed job row for retry
-- with a backoff run_at. Sets status='pending', clears locked_at, and advances run_at so the
-- job re-enters the claim queue at the calculated backoff time. AND status = 'running' ensures
-- the update only applies to rows the scheduler actually leased, guarding against a future
-- double-processor. A 0-row result (e.g. the row was already deleted by Cancel) is harmless
-- and silently tolerated by the caller.
UPDATE jobs SET status = 'pending', run_at = @run_at, locked_at = NULL, updated_at = @updated_at
WHERE id = @id AND status = 'running';

-- name: DeadLetterJob :execrows
-- scopeguard:allow-unscoped: SYSTEM ENGINE -- the scheduler marks an exhausted job as permanently
-- failed. Sets status='failed', records last_error, and clears locked_at. The row is intentionally
-- kept (NOT deleted) so operators can inspect dead-lettered jobs. AND status = 'running' ensures
-- the update only applies to rows the scheduler actually leased, guarding against a future
-- double-processor. A 0-row result (e.g. the row was already deleted by Cancel) is harmless
-- and silently tolerated by the caller.
UPDATE jobs SET status = 'failed', last_error = @last_error, locked_at = NULL, updated_at = @updated_at
WHERE id = @id AND status = 'running';
