-- Articles, attachments, and ticket events: M2 core.
-- The email columns on articles (message_id, in_reply_to, references_header,
-- delivery_status) are reserved for M3 and not written by any M2 query.

-- name: CreateArticle :one
INSERT INTO articles (ticket_id, author_id, sender_type, channel, is_internal, body_text, body_html)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetArticle :one
SELECT * FROM articles
WHERE id = $1;

-- name: ListArticlesByTicket :many
SELECT sqlc.embed(a),
       COALESCE(author.name, '')        AS author_name,
       COALESCE(author.email::text, '') AS author_email
FROM articles a
LEFT JOIN users author ON author.id = a.author_id
WHERE a.ticket_id = $1
ORDER BY a.created_at, a.id;

-- name: SetArticleEmailMeta :one
-- M3 email engine: stamp the email headers (and, for outbound, the
-- delivery status) onto an article inside the ingest/send transaction.
-- Message-IDs are stored canonically (no angle brackets).
UPDATE articles
SET message_id        = sqlc.narg('message_id'),
    in_reply_to       = sqlc.narg('in_reply_to'),
    references_header = sqlc.narg('references_header'),
    delivery_status   = sqlc.narg('delivery_status')
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: SetArticleDeliveryStatus :one
-- Outbound delivery badge transitions: queued -> sending -> sent | failed.
UPDATE articles
SET delivery_status = $2
WHERE id = $1
RETURNING *;

-- name: MarkFailedArticleQueued :one
-- Retry-send (POST /articles/{id}/retry-send): flip a dead delivery back
-- to queued. 'failed' is the terminal badge; 'sending' is accepted only
-- for the stuck-badge case — a crash during the final send attempt whose
-- job River's rescuer discarded without re-running the worker — and the
-- caller (RetryFailedSend) verifies no live send job exists first. The
-- status predicate makes the transition atomic: a concurrent retry sees
-- no row and answers 409 instead of enqueueing a duplicate job.
UPDATE articles
SET delivery_status = 'queued'
WHERE id = $1 AND delivery_status IN ('failed', 'sending')
RETURNING *;

-- name: TicketHasSystemEmailArticle :one
-- Auto-ack idempotency guard: has this ticket already received the PUBLIC
-- system-generated email article (the auto-ack)? is_internal must be
-- filtered: the attachment-drop notice is also sender_type=system +
-- channel=email but internal, and counting it would silently suppress the
-- ack for any new ticket whose first mail blew the attachment cap.
SELECT EXISTS (
    SELECT 1 FROM articles
    WHERE ticket_id = $1 AND sender_type = 'system' AND channel = 'email'
      AND is_internal = false
)::boolean AS has_ack;

-- name: CreateArticleAttachment :one
INSERT INTO article_attachments (article_id, filename, content_type, size_bytes, storage_key)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetArticleAttachment :one
SELECT * FROM article_attachments
WHERE id = $1;

-- name: ListArticleAttachments :many
SELECT * FROM article_attachments
WHERE article_id = $1
ORDER BY created_at, id;

-- name: ListAttachmentsForTicket :many
SELECT aa.* FROM article_attachments aa
JOIN articles a ON a.id = aa.article_id
WHERE a.ticket_id = $1
ORDER BY aa.created_at, aa.id;

-- name: DeleteArticleAttachment :exec
DELETE FROM article_attachments
WHERE id = $1;

-- name: CreateTicketEvent :one
INSERT INTO ticket_events (ticket_id, actor_id, type, payload)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListTicketEvents :many
-- Ordered by the identity pk: insertion order, stable even within one
-- transaction (where created_at ties).
SELECT * FROM ticket_events
WHERE ticket_id = $1
ORDER BY id;
