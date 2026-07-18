-- Mailboxes: M3 email channel configuration + health.

-- name: ListMailboxes :many
-- Health columns (last_poll_at, last_error, last_error_at) ride along for
-- the admin screen's health pills.
SELECT * FROM mailboxes
ORDER BY name, id;

-- name: ListActiveMailboxes :many
-- The poller supervisors' work list.
SELECT * FROM mailboxes
WHERE active
ORDER BY name, id;

-- name: GetMailbox :one
SELECT * FROM mailboxes
WHERE id = $1;

-- name: GetMailboxByEmailAddress :one
SELECT * FROM mailboxes
WHERE email_address = $1;

-- name: CreateMailbox :one
INSERT INTO mailboxes (
    name, email_address, active, auth_kind,
    imap_host, imap_port, imap_tls_mode, imap_username,
    smtp_host, smtp_port, smtp_tls_mode, smtp_username,
    credentials_enc, oauth_tenant_id, oauth_client_id,
    from_display_name, signature, auto_ack_enabled
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, $8,
    $9, $10, $11, $12,
    $13, $14, $15,
    $16, $17, $18
)
RETURNING *;

-- name: UpdateMailbox :one
-- Full-row config update, credentials included. Callers re-encrypt and pass
-- the existing credentials_enc unchanged when the secrets were not edited.
UPDATE mailboxes
SET name              = $2,
    email_address     = $3,
    active            = $4,
    auth_kind         = $5,
    imap_host         = $6,
    imap_port         = $7,
    imap_tls_mode     = $8,
    imap_username     = $9,
    smtp_host         = $10,
    smtp_port         = $11,
    smtp_tls_mode     = $12,
    smtp_username     = $13,
    credentials_enc   = $14,
    oauth_tenant_id   = $15,
    oauth_client_id   = $16,
    from_display_name = $17,
    signature         = $18,
    auto_ack_enabled  = $19,
    updated_at        = now()
WHERE id = $1
RETURNING *;

-- name: TouchMailbox :one
-- Bump updated_at only. The supervisor restarts a mailbox's runner when
-- updated_at moves, so this forces a config/credential reload without
-- rewriting any column (used after the Google OAuth callback stores a
-- refresh token via UpdateMailboxCredentials, which deliberately leaves
-- updated_at alone).
UPDATE mailboxes
SET updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UpdateMailboxCredentials :exec
-- Credentials-only rotation (e.g. cached OAuth access token refresh) that
-- leaves config and updated_at alone.
UPDATE mailboxes
SET credentials_enc = $2
WHERE id = $1;

-- name: DeleteMailbox :exec
DELETE FROM mailboxes
WHERE id = $1;

-- name: SetMailboxHealthOK :exec
-- A successful poll: stamp last_poll_at, clear any error.
UPDATE mailboxes
SET last_poll_at  = now(),
    last_error    = NULL,
    last_error_at = NULL
WHERE id = $1;

-- name: SetMailboxHealthError :exec
-- A failed poll/send: record the error; last_poll_at keeps its old value so
-- "last success" stays visible next to the error.
UPDATE mailboxes
SET last_error    = $2,
    last_error_at = now()
WHERE id = $1;
