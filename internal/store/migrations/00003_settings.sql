-- +goose Up
-- +goose StatementBegin
-- settings stores instance-global operational configuration at runtime.
-- It is a system-owned table (no user_id column) and is exempt from the
-- per-user scoping guard.  Values may contain plain strings or JSON; the
-- caller decides the encoding.  The updated_at column is refreshed on every
-- upsert so consumers can detect staleness without a separate audit trail.
CREATE TABLE settings (
    setting_key TEXT NOT NULL PRIMARY KEY,
    value       TEXT NOT NULL,
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE settings;
-- +goose StatementEnd
