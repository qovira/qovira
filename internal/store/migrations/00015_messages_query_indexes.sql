-- +goose Up
-- +goose StatementBegin
-- Index serving ListMessages: WHERE conversation_id=? AND user_id=? ORDER BY created_at, id. The leading
-- (conversation_id, user_id) columns satisfy the equality predicates; appending (created_at, id) makes the index
-- covering for the ORDER BY, eliminating a USE TEMP B-TREE sort and avoiding a full SCAN of the messages table.
CREATE INDEX messages_conv_user_created ON messages (conversation_id, user_id, created_at, id);
-- +goose StatementEnd

-- +goose StatementBegin
-- Index serving the messages-side aggregation in ListConversations: the derived table groups messages WHERE
-- m.user_id=? AND m.role='user' BY m.conversation_id and picks the earliest created_at||id.  Leading (user_id, role)
-- satisfies the equality filters; appending (conversation_id, created_at) allows SQLite to satisfy the GROUP BY and MIN
-- scan directly from the index without a separate sort or temp-table pass.
CREATE INDEX messages_user_role_conv ON messages (user_id, role, conversation_id, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS messages_user_role_conv;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS messages_conv_user_created;
-- +goose StatementEnd
