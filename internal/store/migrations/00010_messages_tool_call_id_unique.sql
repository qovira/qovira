-- +goose Up
-- +goose StatementBegin
-- Partial unique index on messages(conversation_id, tool_call_id) WHERE tool_call_id IS NOT NULL.
-- Guarantees at most one tool-result row per (conversation, callID) even if some
-- future code path bypasses the per-conversation lock. Non-tool messages have
-- NULL tool_call_id and are exempt from the constraint.
--
-- With the per-conversation run lock in place this index should never fire in
-- practice; it is defense-in-depth only.
CREATE UNIQUE INDEX IF NOT EXISTS messages_conversation_tool_call_id_unique
    ON messages (conversation_id, tool_call_id)
    WHERE tool_call_id IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS messages_conversation_tool_call_id_unique;
-- +goose StatementEnd
