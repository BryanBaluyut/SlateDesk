-- Tags and ticket_tags: M2 core.

-- name: CreateTag :one
INSERT INTO tags (name, color)
VALUES ($1, $2)
RETURNING *;

-- name: GetTag :one
SELECT * FROM tags
WHERE id = $1;

-- name: GetTagByName :one
SELECT * FROM tags
WHERE name = $1;

-- name: ListTags :many
SELECT * FROM tags
ORDER BY name;

-- name: UpdateTag :one
UPDATE tags
SET name  = $2,
    color = $3
WHERE id = $1
RETURNING *;

-- name: DeleteTag :exec
DELETE FROM tags
WHERE id = $1;

-- name: ClearTicketTags :exec
-- Set-ticket-tags is clear + bulk insert inside the service transaction.
DELETE FROM ticket_tags
WHERE ticket_id = $1;

-- name: AddTicketTags :exec
INSERT INTO ticket_tags (ticket_id, tag_id)
SELECT sqlc.arg('ticket_id')::uuid, unnest(sqlc.arg('tag_ids')::uuid[])
ON CONFLICT DO NOTHING;

-- name: ListTagsForTicket :many
SELECT tg.* FROM tags tg
JOIN ticket_tags tt ON tt.tag_id = tg.id
WHERE tt.ticket_id = $1
ORDER BY tg.name;

-- name: CountTagsByIDs :one
-- Existence check used by SetTags to reject unknown tag ids up front.
SELECT count(*) FROM tags
WHERE id = ANY (sqlc.arg('tag_ids')::uuid[]);
