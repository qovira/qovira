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
-- scopeguard:allow-unscoped: the outer SELECT carries c.user_id = @user_id
-- (conversations is fully scoped), and the messages derived table carries
-- m.user_id = @user_id independently. The scope guard fails closed on any JOIN
-- or SELECT-inside-SELECT; this reviewed exemption documents that both target
-- tables are correctly user-scoped.
--
-- List conversations for a user, keyset-paginated on (updated_at DESC, id DESC)
-- so the most-recently-active conversation appears first. Fetches limit+1 rows so
-- the caller can detect whether a next page exists.
--
-- preview is the conversation's first user message, derived in a LEFT JOIN to a
-- per-conversation derived table rather than the obvious correlated subquery in
-- the projection. sqlc's SQLite parser rejects a bound parameter inside a
-- projection subquery (it substitutes positional placeholders and re-parses, and
-- such a placeholder inside that subquery is invalid to its grammar), and it has
-- no window-function support, so a ROW_NUMBER ranking is out too. The derived
-- table instead groups by conversation and relies on SQLite's documented rule
-- that, with exactly one MIN in the SELECT, the remaining bare columns are taken
-- from the row holding that minimum. The MIN key is created_at concatenated with
-- id; created_at is a fixed-width ISO-8601 string, so that key sorts identically
-- to ordering by created_at then id, making fm.content the earliest user message
-- deterministically with ties broken by id, exactly as the previous
-- ORDER BY m.created_at, m.id LIMIT 1 did. m.user_id = @user_id keeps messages
-- user-scoped independently of the join (defense in depth; the conversation_id +
-- user_id FK already ties each message's user_id to its conversation's owner).
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
    COALESCE(fm.content, '') AS preview
FROM conversations c
LEFT JOIN (
    SELECT
        m.conversation_id AS conversation_id,
        m.content AS content,
        MIN(m.created_at || m.id) AS first_key
    FROM messages m
    WHERE m.user_id = @user_id
      AND m.role = 'user'
    GROUP BY m.conversation_id
) fm ON fm.conversation_id = c.id
WHERE c.user_id = @user_id
  AND (
      sqlc.narg('cursor_updated_at') IS NULL
      OR c.updated_at < sqlc.narg('cursor_updated_at')
      OR (c.updated_at = sqlc.narg('cursor_updated_at') AND c.id < sqlc.narg('cursor_id'))
  )
ORDER BY c.updated_at DESC, c.id DESC
LIMIT @limit;
