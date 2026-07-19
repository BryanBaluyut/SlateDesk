-- API keys: scoped machine credentials (M4). The plaintext key is never
-- stored; only its SHA-256 hex digest (key_hash) is, and authentication is
-- an exact-match lookup on that unique digest.

-- name: CreateAPIKey :one
INSERT INTO api_keys (name, key_prefix, key_hash, scopes, created_by)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetAPIKeyForAuth :one
-- Authentication lookup: resolve a presented key's SHA-256 digest to the key
-- (id, scopes) AND the acting user it was minted by, in one round trip. Only
-- live (non-revoked) keys match. last_used_at is deliberately NOT touched
-- here — call TouchAPIKey separately so the hot auth path stays a pure read.
SELECT
    k.id           AS api_key_id,
    k.name         AS api_key_name,
    k.scopes       AS scopes,
    k.created_at   AS api_key_created_at,
    u.id           AS user_id,
    u.email        AS user_email,
    u.name         AS user_name,
    u.role         AS user_role,
    u.token_version AS user_token_version,
    u.active       AS user_active
FROM api_keys k
JOIN users u ON u.id = k.created_by
WHERE k.key_hash = $1
  AND k.revoked_at IS NULL;

-- name: TouchAPIKey :exec
-- Stamp last_used_at after a successful authenticated request. Kept separate
-- from GetAPIKeyForAuth so it can be fired best-effort off the request path.
-- Throttled to at most one write per key per minute: last_used_at is a coarse
-- "recently seen" signal, not an access log, so a busy key polling at high QPS
-- must not generate one same-row UPDATE (and dead-tuple churn) per request.
UPDATE api_keys
SET last_used_at = now()
WHERE id = $1
  AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: ListAPIKeys :many
-- Admin CRUD listing. Secrets never appear (no key_hash); the display prefix
-- identifies each row. Live keys first, then revoked, newest first.
SELECT id, name, key_prefix, scopes, created_by, last_used_at, created_at, revoked_at
FROM api_keys
ORDER BY (revoked_at IS NOT NULL), created_at DESC;

-- name: GetAPIKey :one
SELECT id, name, key_prefix, scopes, created_by, last_used_at, created_at, revoked_at
FROM api_keys
WHERE id = $1;

-- name: RevokeAPIKey :execrows
-- Idempotent revoke: only flips a currently-live key. Zero rows affected =
-- unknown or already revoked, which the handler maps to 404/no-op.
UPDATE api_keys
SET revoked_at = now()
WHERE id = $1
  AND revoked_at IS NULL;
