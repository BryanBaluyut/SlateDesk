-- Webhooks: outbound HTTP endpoints (M4). secret_enc holds the AES-GCM
-- ciphertext of the per-endpoint HMAC signing secret (secrets package,
-- purpose "webhook-secret"); it is written and read as opaque text here.

-- name: CreateWebhook :one
INSERT INTO webhooks (name, url, secret_enc, events, active)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetWebhook :one
SELECT * FROM webhooks
WHERE id = $1;

-- name: ListWebhooks :many
SELECT * FROM webhooks
ORDER BY created_at DESC, id;

-- name: ListActiveWebhooksByEvent :many
-- The delivery fan-out: every active endpoint subscribed to this event type.
-- Drives transactional enqueue of webhook_deliveries off a ticket_event.
SELECT * FROM webhooks
WHERE active
  AND $1::text = ANY (events)
ORDER BY id;

-- name: UpdateWebhook :one
-- Full-row config update. Callers pass the existing secret_enc unchanged when
-- the signing secret was not rotated.
UPDATE webhooks
SET name       = $2,
    url        = $3,
    secret_enc = $4,
    events     = $5,
    active     = $6,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteWebhook :execrows
DELETE FROM webhooks
WHERE id = $1;
