-- name: UpsertSetting :exec
-- Upsert a setting by key.  Inserts a new row or replaces value and
-- updated_at when the key already exists.
INSERT INTO settings (setting_key, value, updated_at)
VALUES (@setting_key, @value, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
ON CONFLICT(setting_key) DO UPDATE
    SET value      = excluded.value,
        updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now');

-- name: ListSettingsByPrefix :many
-- List all settings whose key starts with @prefix, ordered by key. The caller
-- must escape LIKE metacharacters (\, %, _) in @prefix; ESCAPE '\' then makes
-- those escapes literal, so a prefix containing '_' or '%' matches literally
-- rather than as a wildcard.
SELECT setting_key, value, updated_at
FROM settings
WHERE setting_key LIKE @prefix || '%' ESCAPE '\'
ORDER BY setting_key;

-- name: GetSetting :one
-- Retrieve a single setting row by its exact key.
SELECT setting_key, value, updated_at
FROM settings
WHERE setting_key = @setting_key;

-- name: DeleteSetting :exec
-- Delete a setting by key.  No-op when the key does not exist.
DELETE FROM settings
WHERE setting_key = @setting_key;
