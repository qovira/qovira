-- +goose Up
-- +goose StatementBegin
-- Add an abandoned flag to messages. An assistant message is marked abandoned when
-- its pending confirmation expires before the user answers: the tool does not execute,
-- the model round is NOT re-entered (expiry is asymmetric with deny), and the flag
-- makes the turn inert — outstandingToolCalls and isTurnComplete treat abandoned
-- assistant messages as terminal so the conversation never hangs waiting for results.
ALTER TABLE messages ADD COLUMN abandoned INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- SQLite does not support DROP COLUMN on tables with certain constraints; recreate the
-- table without the abandoned column.
CREATE TABLE messages_v10 (
    id              TEXT NOT NULL PRIMARY KEY,
    conversation_id TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    role            TEXT NOT NULL,
    content         TEXT NOT NULL,
    tool_calls      TEXT,
    tool_call_id    TEXT,
    finish_reason   TEXT,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
INSERT INTO messages_v10 SELECT id, conversation_id, user_id, role, content, tool_calls, tool_call_id, finish_reason, created_at FROM messages;
DROP TABLE messages;
ALTER TABLE messages_v10 RENAME TO messages;
CREATE UNIQUE INDEX IF NOT EXISTS messages_conversation_tool_call_id_unique
    ON messages (conversation_id, tool_call_id)
    WHERE tool_call_id IS NOT NULL;
-- +goose StatementEnd
