-- Canned replies: reusable agent snippets (M4). Variable substitution
-- ({{ticket.number}}, {{requester.name}}) happens in the service layer when a
-- reply is inserted into the composer; the stored body keeps the raw template.

-- name: CreateCannedReply :one
INSERT INTO canned_replies (title, body, created_by)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetCannedReply :one
SELECT * FROM canned_replies
WHERE id = $1;

-- name: ListCannedReplies :many
SELECT * FROM canned_replies
ORDER BY title, id;

-- name: UpdateCannedReply :one
UPDATE canned_replies
SET title      = $2,
    body       = $3,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteCannedReply :execrows
DELETE FROM canned_replies
WHERE id = $1;
