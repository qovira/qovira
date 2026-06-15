-- +goose Up
-- +goose StatementBegin
-- Index on (user_id, updated_at, id): serves the ListConversations keyset query
-- ordered by (updated_at DESC, id DESC) — most-recently-active first.
-- The index satisfies ORDER BY updated_at DESC, id DESC directly from the
-- index-ordered stream when scanned in reverse, avoiding a USE TEMP B-TREE.
CREATE INDEX conversations_user_updated ON conversations (user_id, updated_at, id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS conversations_user_updated;
-- +goose StatementEnd
