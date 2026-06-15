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

-- name: ListConversations :many
-- scopeguard:allow-unscoped: the WHERE clause on the outer SELECT carries
-- c.user_id = @user_id (conversations is fully scoped), and the correlated
-- subquery on messages carries m.user_id = @user_id independently. The scope
-- guard fails closed on any SELECT-inside-SELECT; this reviewed exemption
-- documents that both target tables are correctly user-scoped.
--
-- List conversations for a user, keyset-paginated on (updated_at DESC, id DESC)
-- so the most-recently-active conversation appears first. Fetches limit+1 rows so
-- the caller can detect whether a next page exists.
--
-- preview is derived via a correlated subquery that finds the first user message
-- in the conversation (ORDER BY created_at, id LIMIT 1). The subquery carries its
-- own user_id predicate (m.user_id = @user_id) keeping messages scoped to the
-- calling user — no cross-user message leakage is possible.
--
-- The keyset predicate uses the expanded tuple form (required because sqlc's SQLite
-- parser does not support row-value syntax): the cursor marks the last seen
-- (updated_at, id) pair and we skip rows that sort before it in the DESC order.
-- Equivalent to: (updated_at, id) < (cursor_updated_at, cursor_id) in DESC order,
-- which expands to:
--   updated_at < cursor OR (updated_at = cursor AND id < cursor_id).
SELECT
    c.id,
    c.created_at,
    c.updated_at,
    COALESCE((
        SELECT m.content
        FROM messages m
        WHERE m.conversation_id = c.id
          AND m.user_id = @user_id
          AND m.role = 'user'
        ORDER BY m.created_at, m.id
        LIMIT 1
    ), '') AS preview
FROM conversations c
WHERE c.user_id = @user_id
  AND (
      sqlc.narg('cursor_updated_at') IS NULL
      OR c.updated_at < sqlc.narg('cursor_updated_at')
      OR (c.updated_at = sqlc.narg('cursor_updated_at') AND c.id < sqlc.narg('cursor_id'))
  )
ORDER BY c.updated_at DESC, c.id DESC
LIMIT @limit;
