-- Queries for the conversations table.
-- Every SELECT/UPDATE/DELETE includes a user_id predicate so the row always
-- belongs to the bound Scope. Parameters use sqlc named params (@name).

-- name: UpsertConversation :exec
-- Insert-if-new only. The store wrapper (conversations.go) enforces ownership by
-- calling GetConversation immediately after: if the INSERT no-opped because the id
-- belongs to another user, GetConversation (user-scoped) returns sql.ErrNoRows,
-- which is mapped to ErrConversationNotOwned. When the caller re-posts to their
-- own conversation, GetConversation finds the row and TouchConversation bumps
-- updated_at. This three-step protocol is safe because the write pool is capped at
-- one connection, serialising all writes and eliminating TOCTOU races.
INSERT INTO conversations (id, user_id, created_at, updated_at)
VALUES (@id, @user_id, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
ON CONFLICT (id) DO NOTHING;

-- name: GetConversation :one
SELECT id, user_id, summary, created_at, updated_at
FROM conversations
WHERE id = @id
  AND user_id = @user_id;

-- name: TouchConversation :exec
UPDATE conversations
SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = @id
  AND user_id = @user_id;
