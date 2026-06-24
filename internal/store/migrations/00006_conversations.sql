-- +goose Up
-- +goose StatementBegin
-- conversations holds one row per conversation thread, owned by a specific user.
-- summary stays NULL for this slice (populated by a later summarisation feature).
CREATE TABLE conversations (
    id         TEXT NOT NULL PRIMARY KEY,  -- ULID TEXT(26)
    user_id    TEXT NOT NULL,              -- owner; references users(id) logically
    summary    TEXT,                       -- NULL until summarised
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT, WITHOUT ROWID;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE conversations;
-- +goose StatementEnd
