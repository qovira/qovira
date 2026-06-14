-- Queries for the messages table.
-- Every SELECT/UPDATE/DELETE includes a user_id predicate so the row always
-- belongs to the bound Scope. Parameters use sqlc named params (@name).

-- name: InsertMessage :one
INSERT INTO messages (id, conversation_id, user_id, role, content, tool_calls, tool_call_id, finish_reason)
VALUES (@id, @conversation_id, @user_id, @role, @content, @tool_calls, @tool_call_id, @finish_reason)
RETURNING id, conversation_id, user_id, role, content, tool_calls, tool_call_id, finish_reason, created_at;

-- name: ListMessages :many
SELECT id, conversation_id, user_id, role, content, tool_calls, tool_call_id, finish_reason, created_at
FROM messages
WHERE conversation_id = @conversation_id
  AND user_id = @user_id
ORDER BY created_at;
