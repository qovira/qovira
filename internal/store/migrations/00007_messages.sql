-- +goose Up
-- +goose StatementBegin
-- messages stores the OpenAI-shaped message history for each conversation.
-- Each row is owned by a user and belongs to one conversation.
-- tool_calls is a JSON array (nullable); tool_call_id and finish_reason are
-- nullable text fields used for tool-result and assistant rows respectively.
CREATE TABLE messages (
    id              TEXT NOT NULL PRIMARY KEY,  -- ULID TEXT(26)
    conversation_id TEXT NOT NULL,              -- references conversations(id)
    user_id         TEXT NOT NULL,              -- owner; denormalised for scope-guard
    role            TEXT NOT NULL,              -- 'system' | 'user' | 'assistant' | 'tool'
    content         TEXT NOT NULL,
    tool_calls      TEXT,                       -- JSON array or NULL
    tool_call_id    TEXT,                       -- NULL unless role='tool'
    finish_reason   TEXT,                       -- NULL unless role='assistant'
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE messages;
-- +goose StatementEnd
