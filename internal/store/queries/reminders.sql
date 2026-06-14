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
