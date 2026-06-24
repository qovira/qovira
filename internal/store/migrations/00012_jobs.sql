-- +goose Up
-- +goose StatementBegin
-- jobs is the durable job queue for the scheduler. It is privately owned by internal/scheduler;
-- no other package may read or write this table directly. Producers reach it only via Enqueue;
-- the scheduler engine owns the claim, execution, and deletion paths.
--
-- Design notes:
--   - id: ULID (TEXT) primary key — lexicographically sortable, unique, opaque.
--   - key: optional idempotency handle. When set, ON CONFLICT(key) DO NOTHING on INSERT
--     ensures a given logical job is never enqueued twice.
--   - kind: the dispatch selector — maps to a registered Handler.
--   - payload: opaque JSON text; the scheduler passes it through to the handler unchanged.
--   - user_id: nullable. NULL = system scope; non-null = user-scoped job.
--   - status: pending → running → (deleted on success, failed on permanent failure).
--   - run_at: RFC 3339 UTC TEXT. The claim query drives the (status, run_at) index.
--   - attempt: 0 until first lease; incremented atomically with the status transition at claim.
--   - locked_at: set to the claim timestamp when running; NULL otherwise.
--   - rrule, tz, interval_secs: recurrence columns — created now, unused until the recurrence slice.
--   - last_error: most recent failure text — created now, unused until the retry slice.
--   - created_at / updated_at: RFC 3339 UTC TEXT, default to the current moment.
CREATE TABLE jobs (
    id             TEXT NOT NULL PRIMARY KEY,
    key            TEXT,
    kind           TEXT NOT NULL,
    payload        TEXT NOT NULL DEFAULT '{}',
    user_id        TEXT,
    status         TEXT NOT NULL DEFAULT 'pending',
    run_at         TEXT NOT NULL,
    attempt        INTEGER NOT NULL DEFAULT 0,
    locked_at      TEXT,
    rrule          TEXT,
    tz             TEXT,
    interval_secs  INTEGER,
    last_error     TEXT,
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT, WITHOUT ROWID;
-- +goose StatementEnd

-- +goose StatementBegin
-- Unique index on key: enforces the idempotency / singleton-handle contract.
-- Partial index (WHERE key IS NOT NULL) so that multiple NULL-key (fire-and-forget) rows are allowed.
CREATE UNIQUE INDEX jobs_key_unique ON jobs (key) WHERE key IS NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
-- Index on (status, run_at): the claim query drives this index to find due pending jobs
-- (WHERE status='pending' AND run_at <= @now ORDER BY run_at LIMIT @batch).
-- Note: this index does NOT cover the reclaim path — see jobs_running_locked_at below.
CREATE INDEX jobs_status_run_at ON jobs (status, run_at);
-- +goose StatementEnd

-- +goose StatementBegin
-- Partial index on locked_at for the reclaim sweep: ReclaimStaleJobs runs on every poll tick
-- (WHERE status='running' AND locked_at IS NOT NULL AND locked_at < @threshold).
-- Without this index the threshold comparison is a per-row scan of all running rows.
-- The partial predicate (WHERE status = 'running') keeps the index small and focused.
CREATE INDEX jobs_running_locked_at ON jobs (locked_at) WHERE status = 'running';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS jobs_running_locked_at;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS jobs_status_run_at;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS jobs_key_unique;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE jobs;
-- +goose StatementEnd
