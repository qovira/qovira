-- Scoped queries for the reminders table.
-- reminders is user-owned: every SELECT/UPDATE/DELETE includes a user_id
-- predicate so rows are always confined to the bound Scope.  The CI scope
-- guard (TestScopeGuard_RealQueries) enforces this at build time.
--
-- Parameters use sqlc named params (@name) per the house convention.

-- name: InsertReminder :exec
INSERT INTO reminders (
    id, user_id, title, notes, due_at, rrule, tz,
    auto_complete, status, completed_at, last_fired_at,
    fire_job_id, created_at, updated_at
) VALUES (
    @id, @user_id, @title, @notes, @due_at, @rrule, @tz,
    @auto_complete, @status, @completed_at, @last_fired_at,
    @fire_job_id, @created_at, @updated_at
);

-- name: GetReminder :one
SELECT id, user_id, title, notes, due_at, rrule, tz,
       auto_complete, status, completed_at, last_fired_at,
       fire_job_id, created_at, updated_at
FROM reminders
WHERE id = @id
  AND user_id = @user_id;

-- name: StampFiredAutoComplete :execrows
UPDATE reminders
SET last_fired_at = @last_fired_at,
    status        = 'completed',
    completed_at  = @completed_at,
    updated_at    = @updated_at
WHERE id = @id
  AND user_id = @user_id;

-- name: StampFiredKeepActive :execrows
UPDATE reminders
SET last_fired_at = @last_fired_at,
    updated_at    = @updated_at
WHERE id = @id
  AND user_id = @user_id;

-- name: SetReminderFireJobID :execrows
UPDATE reminders
SET fire_job_id = @fire_job_id,
    updated_at  = @updated_at
WHERE id = @id
  AND user_id = @user_id;

-- name: DeleteReminder :execrows
DELETE FROM reminders
WHERE id      = @id
  AND user_id = @user_id;

-- name: ListReminders :many
-- List reminders for a user with optional status and due-window filters,
-- keyset-paginated on (due_at, id).  Fetches limit+1 rows so the caller can
-- detect whether a next page exists.
--
-- Optional filters use sqlc.narg so absent values are NULL and the predicate
-- becomes a no-op.  The keyset predicate uses the expanded form of the tuple
-- comparison (due_at, id) > (cursor_due, cursor_id), which is logically
-- identical: skip rows that sort before the cursor.  Both forms produce the
-- same result set; the expanded form is required because sqlc's SQLite parser
-- does not recognise the row-value syntax (col, col) > (?, ?).
--
-- The query is served by the reminders_user_due index on (user_id, due_at, id).
-- That index satisfies ORDER BY due_at, id directly from the index-ordered stream
-- for both the no-status path and the status-filtered path (status is a residual
-- predicate).  No USE TEMP B-TREE FOR ORDER BY occurs in either case, verified
-- by EXPLAIN QUERY PLAN in TestListReminders_IndexPlan.
SELECT id, user_id, title, notes, due_at, rrule, tz,
       auto_complete, status, completed_at, last_fired_at,
       fire_job_id, created_at, updated_at
FROM reminders
WHERE user_id = @user_id
  AND (sqlc.narg('status')     IS NULL OR status = sqlc.narg('status'))
  AND (sqlc.narg('due_after')  IS NULL OR due_at > sqlc.narg('due_after'))
  AND (sqlc.narg('due_before') IS NULL OR due_at < sqlc.narg('due_before'))
  AND (sqlc.narg('cursor_due') IS NULL
       OR due_at > sqlc.narg('cursor_due')
       OR (due_at = sqlc.narg('cursor_due') AND id > sqlc.narg('cursor_id')))
ORDER BY due_at, id
LIMIT @limit;
