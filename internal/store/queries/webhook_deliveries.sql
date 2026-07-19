-- Webhook deliveries: the durable outbox log (M4). A row is inserted
-- 'pending' in the same transaction as the ticket_event that spawned it; a
-- River job then attempts HTTP delivery and records the outcome. Attempts is
-- incremented on every terminal or intermediate outcome so the delivery log
-- reflects River's retry count.

-- name: InsertWebhookDelivery :one
-- Enqueue a pending delivery (status defaults to 'pending', attempts 0).
INSERT INTO webhook_deliveries (webhook_id, event_type, ticket_id, payload)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetWebhookDelivery :one
SELECT * FROM webhook_deliveries
WHERE id = $1;

-- name: MarkWebhookDeliverySuccess :exec
-- Terminal success: 2xx received. Stamp delivered_at, clear any prior error.
UPDATE webhook_deliveries
SET status        = 'success',
    response_code = $2,
    attempts      = attempts + 1,
    last_error    = NULL,
    delivered_at  = now()
WHERE id = $1;

-- name: MarkWebhookDeliveryFailed :exec
-- Terminal failure: River has exhausted its retries. response_code is null
-- for transport-level errors (no HTTP response).
UPDATE webhook_deliveries
SET status        = 'failed',
    response_code = $2,
    attempts      = attempts + 1,
    last_error    = $3
WHERE id = $1;

-- name: RecordWebhookDeliveryRetry :exec
-- Intermediate failure with a retry still scheduled: keep status 'pending',
-- but bump attempts and record the latest error/code so the log shows
-- progress between River attempts (avoids a spurious failed->success flip).
UPDATE webhook_deliveries
SET status        = 'pending',
    response_code = $2,
    attempts      = attempts + 1,
    last_error    = $3
WHERE id = $1;

-- name: ListRecentWebhookDeliveries :many
-- Admin delivery log for one endpoint, newest first.
SELECT * FROM webhook_deliveries
WHERE webhook_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2;
