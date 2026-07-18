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
