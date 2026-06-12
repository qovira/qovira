-- +goose Up
-- +goose StatementBegin
-- users stores the identity record for every Qovira account.  It is a
-- system-owned table (no user_id column) because it is the identity table from
-- which per-user scope is derived; it cannot itself be user-scoped.  It is
-- exempt from the per-user scoping guard (see scopeguard.go).  Email is
-- normalised (trimmed + lower-cased) before storage; the UNIQUE constraint
-- therefore enforces case-insensitive uniqueness at the DB level.
CREATE TABLE users (
    id            TEXT NOT NULL PRIMARY KEY,    -- ULID
    email         TEXT NOT NULL UNIQUE,         -- normalised: trimmed + lower-cased
    display_name  TEXT NOT NULL,
    password_hash TEXT NOT NULL,                -- argon2id PHC string
    role          TEXT NOT NULL,                -- 'admin' | 'member'
    timezone      TEXT NOT NULL,                -- IANA
    locale        TEXT NOT NULL,                -- BCP 47
    language      TEXT NOT NULL,                -- BCP 47
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE users;
-- +goose StatementEnd
