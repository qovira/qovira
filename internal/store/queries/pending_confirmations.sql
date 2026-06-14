-- Queries for the pending_confirmations table.
-- Every SELECT/UPDATE includes a user_id predicate so the row always
-- belongs to the bound Scope. Parameters use sqlc named params (@name).

-- name: InsertPendingConfirmation :one
INSERT INTO pending_confirmations (id, conversation_id, message_id, user_id, tool_name, args, risk, status, expires_at)
VALUES (@id, @conversation_id, @message_id, @user_id, @tool_name, @args, @risk, @status, @expires_at)
RETURNING id, conversation_id, message_id, user_id, tool_name, args, risk, status, created_at, expires_at;

-- name: GetPendingConfirmation :one
SELECT id, conversation_id, message_id, user_id, tool_name, args, risk, status, created_at, expires_at
FROM pending_confirmations
WHERE id = @id
  AND user_id = @user_id;

-- name: UpdatePendingConfirmationStatus :execrows
UPDATE pending_confirmations
SET status = @status
WHERE id = @id
  AND user_id = @user_id
  AND status = 'pending';

-- name: ListPendingConfirmationsByConversation :many
SELECT id, conversation_id, message_id, user_id, tool_name, args, risk, status, created_at, expires_at
FROM pending_confirmations
WHERE conversation_id = @conversation_id
  AND user_id = @user_id
ORDER BY created_at, id;
