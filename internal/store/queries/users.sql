-- Queries for the users identity table.
-- users is system-owned (no user_id column; it is the identity table from which
-- per-user scope is derived) and is exempt from the scope guard.  All
-- SELECT/UPDATE operations are therefore permitted without a user_id predicate.
--
-- Parameters use sqlc named params (@name) per the house convention.

-- name: CreateUser :exec
INSERT INTO users (id, email, display_name, password_hash, role, timezone, locale, language, created_at, updated_at)
VALUES (@id, @email, @display_name, @password_hash, @role, @timezone, @locale, @language, @created_at, @updated_at);

-- name: GetUserByEmail :one
SELECT id, email, display_name, password_hash, role, timezone, locale, language, created_at, updated_at
FROM users
WHERE email = @email;

-- name: GetUserByID :one
SELECT id, email, display_name, password_hash, role, timezone, locale, language, created_at, updated_at
FROM users
WHERE id = @id;

-- name: UpdateUserProfile :execrows
UPDATE users
SET display_name = @display_name,
    timezone     = @timezone,
    locale       = @locale,
    language     = @language,
    updated_at   = @updated_at
WHERE id = @id;

-- name: UpdateUserPasswordHash :execrows
UPDATE users
SET password_hash = @password_hash,
    updated_at    = @updated_at
WHERE id = @id;
