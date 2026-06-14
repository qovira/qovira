-- +goose Up
-- +goose StatementBegin
-- reminders stores user-owned reminder records.  Each row is bound to a
-- specific user (user_id NOT NULL) and is never accessible to other users.
-- The scope guard enforces this: reminders is NOT in the systemTables allowlist,
-- so every SELECT/UPDATE/DELETE query on this table must include a user_id
-- predicate or TestScopeGuard_RealQueries will fail the build.
--
-- Design notes:
--   - id: ULID (TEXT) primary key — lexicographically sortable, unique, opaque.
--   - user_id: owning user; every query filters on it.
--   - title: required; non-empty trimmed text (enforced in Service, not DB).
--   - notes: optional free-text annotation.
--   - due_at: RFC 3339 UTC; the next fire instant.  Past values are allowed.
--   - rrule: optional RFC 5545 RRULE string; NULL = one-shot.  Column exists
--     now; recurrence logic is a later slice.
--   - tz: IANA timezone snapshotted at creation (defaulted from user profile).
--   - auto_complete: boolean (1 = complete on fire, 0 = stay active).
--   - status: user intent ('active' | 'completed').  Never a delivery state.
--   - completed_at: set only by explicit complete (later slice); NULL here.
--   - last_fired_at: stamped each time the fire handler runs.
--   - fire_job_id: scheduler job id for the pending fire job (nullable).
--   - created_at / updated_at: RFC 3339 UTC.
CREATE TABLE reminders (
    id             TEXT    NOT NULL PRIMARY KEY,
    user_id        TEXT    NOT NULL,
    title          TEXT    NOT NULL,
    notes          TEXT,
    due_at         TEXT    NOT NULL,
    rrule          TEXT,
    tz             TEXT    NOT NULL,
    auto_complete  INTEGER NOT NULL DEFAULT 1,
    status         TEXT    NOT NULL DEFAULT 'active',
    completed_at   TEXT,
    last_fired_at  TEXT,
    fire_job_id    TEXT,
    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
-- Composite index on (user_id, status, due_at): the primary read pattern is
-- listing a user's active reminders ordered by due date.
CREATE INDEX reminders_user_status_due ON reminders (user_id, status, due_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS reminders_user_status_due;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE reminders;
-- +goose StatementEnd
