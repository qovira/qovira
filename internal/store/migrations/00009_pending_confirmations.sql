-- +goose Up
-- +goose StatementBegin
-- pending_confirmations holds one row per confirmation-required tool call. The row
-- is created when a Confirm-tier tool call is encountered; it is updated (status
-- approved|denied|expired) when the user resolves it via POST .../confirmations/{id}.
-- The ID is the gateway tool call ID (a ULID-shaped string) so it is directly
-- addressable via the API without a separate lookup.
--
-- The composite FOREIGN KEY (conversation_id, user_id) REFERENCES conversations(id,
-- user_id) mirrors the messages backstop: it enforces at the DB level that a
-- pending_confirmation row can only be inserted for a conversation owned by the
-- matching user. The target index conversations_id_user_id was added in migration
-- 00008 and satisfies the UNIQUE constraint required by SQLite for composite FKs.
CREATE TABLE pending_confirmations (
    id              TEXT NOT NULL PRIMARY KEY,     -- gateway tool call ID (ULID); API-addressable
    conversation_id TEXT NOT NULL,                 -- references conversations(id) logically
    message_id      TEXT NOT NULL,                 -- assistant message holding the tool_calls
    user_id         TEXT NOT NULL,                 -- owner; scope-guard predicate on every SELECT/UPDATE
    tool_name       TEXT NOT NULL,                 -- name of the tool requiring confirmation
    args            TEXT NOT NULL,                 -- JSON arguments from the tool call
    risk            TEXT NOT NULL,                 -- risk tier string (read|write|external|destructive)
    status          TEXT NOT NULL DEFAULT 'pending',  -- pending|approved|denied|expired
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    expires_at      TEXT NOT NULL,                 -- RFC 3339 UTC; set to now+ConfirmationTTL on insert
    FOREIGN KEY (conversation_id, user_id) REFERENCES conversations (id, user_id)
) STRICT, WITHOUT ROWID;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE pending_confirmations;
-- +goose StatementEnd
