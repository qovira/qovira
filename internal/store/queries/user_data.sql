-- Scoped queries for the user_data exemplar table.
-- Every SELECT/UPDATE/DELETE includes a user_id predicate so the row always comes from and is limited to the
-- bound Scope. This pattern is the template that real domain tables must follow; the CI guard in scopeguard.go
-- enforces it at build time.
--
-- Parameters use sqlc named params (@name) per the house convention; the generated Params structs carry typed
-- fields (ID, UserID, Value).

-- name: InsertUserData :exec
INSERT INTO user_data (id, user_id, value)
VALUES (@id, @user_id, @value);

-- name: GetUserData :one
SELECT id, user_id, value, created_at
FROM user_data
WHERE id = @id
  AND user_id = @user_id;

-- name: ListUserData :many
SELECT id, user_id, value, created_at
FROM user_data
WHERE user_id = @user_id
ORDER BY created_at;

-- name: DeleteUserData :exec
DELETE FROM user_data
WHERE id = @id
  AND user_id = @user_id;
