-- +goose Up
-- +goose StatementBegin
-- user_data is a scoping-exemplar table. It exists solely to exercise the
-- per-user data-isolation boundary (Scope / ScopedQueries) and the CI guard
-- in scopeguard.go. It is NOT a domain entity — sibling specs that own
-- real domain tables (notes, tasks, etc.) will add their own migrations. As
-- new domain tables are added, they must appear in the scope guard's allowlist
-- or carry a user_id predicate in every SELECT/UPDATE/DELETE query.
CREATE TABLE user_data (
    id         TEXT    NOT NULL PRIMARY KEY,   -- ULID
    user_id    TEXT    NOT NULL,
    value      TEXT    NOT NULL,
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE user_data;
-- +goose StatementEnd
