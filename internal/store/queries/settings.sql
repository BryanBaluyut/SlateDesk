-- Settings: key/value with jsonb values.

-- name: GetSetting :one
SELECT * FROM settings
WHERE key = $1;

-- name: ListSettings :many
SELECT * FROM settings
ORDER BY key;

-- name: SetSetting :exec
INSERT INTO settings (key, value)
VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE
SET value = EXCLUDED.value;

-- name: SetSettingIfAbsent :execrows
INSERT INTO settings (key, value)
VALUES ($1, $2)
ON CONFLICT (key) DO NOTHING;

-- name: DeleteSetting :exec
DELETE FROM settings
WHERE key = $1;
