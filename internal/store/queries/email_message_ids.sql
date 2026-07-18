-- email_message_ids: every inbound and outbound Message-ID (dedup +
-- threading, architecture doc §4).

-- name: InsertEmailMessageID :execrows
-- Returns 0 rows on Message-ID conflict: an inbound redelivery. Callers
-- treat 0 as "already ingested" and skip the message — this is what makes
-- at-least-once IMAP delivery exactly-once.
INSERT INTO email_message_ids (message_id, direction, mailbox_id, ticket_id, article_id)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (message_id) DO NOTHING;

-- name: GetEmailMessageID :one
SELECT * FROM email_message_ids
WHERE message_id = $1;

-- name: LatestInboundMessageIDForTicket :one
-- The Message-ID an outbound reply answers (In-Reply-To header).
SELECT * FROM email_message_ids
WHERE ticket_id = $1 AND direction = 'inbound'
ORDER BY created_at DESC, message_id
LIMIT 1;

-- name: LatestInboundMailboxForTicket :one
-- The mailbox a ticket's email conversation lives on: the mailbox that
-- received the most recent inbound message. No row = not an email-origin
-- ticket (or its mailbox was deleted).
SELECT m.* FROM email_message_ids e
JOIN mailboxes m ON m.id = e.mailbox_id
WHERE e.ticket_id = $1 AND e.direction = 'inbound'
ORDER BY e.created_at DESC, e.message_id
LIMIT 1;

-- name: ListRecentEmailMessageIDsByTicket :many
-- Newest-first with a cap; used to walk References chains (threading) and
-- to build the References header for outbound replies (callers reverse
-- into oldest-first header order).
SELECT * FROM email_message_ids
WHERE ticket_id = $1
ORDER BY created_at DESC, message_id
LIMIT $2;
