-- Tickets: M2 core. Number allocation, CRUD, filtered lists, FTS search,
-- field updates (each RETURNING the row so the service can emit events),
-- and dashboard counters.

-- name: NextTicketNumber :one
-- Race-free per-day sequence: the UPSERT takes a row lock on the day's
-- counter, so concurrent allocations (any number of replicas) serialize
-- and each caller gets a unique, gapless-within-commit value. Call inside
-- the ticket-creating transaction so an aborted create at worst leaves a
-- gap, never a duplicate.
INSERT INTO ticket_number_counters (day, counter)
VALUES (sqlc.arg('day')::date, 1)
ON CONFLICT (day) DO UPDATE
SET counter = ticket_number_counters.counter + 1
RETURNING counter;

-- name: CreateTicket :one
INSERT INTO tickets (number, subject, status, priority, requester_id, assignee_id, team_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetTicket :one
-- Ticket with requester and assignee display fields joined in.
SELECT sqlc.embed(t),
       requester.name  AS requester_name,
       requester.email AS requester_email,
       COALESCE(assignee.name, '')         AS assignee_name,
       COALESCE(assignee.email::text, '')  AS assignee_email
FROM tickets t
JOIN users requester ON requester.id = t.requester_id
LEFT JOIN users assignee ON assignee.id = t.assignee_id
WHERE t.id = $1;

-- name: GetTicketForUpdate :one
-- Row-locked read used inside service transactions that decide on a state
-- change (e.g. the add-article status matrix) so concurrent writers
-- serialize instead of racing.
SELECT * FROM tickets
WHERE id = $1
FOR UPDATE;

-- name: GetTicketByNumber :one
SELECT sqlc.embed(t),
       requester.name  AS requester_name,
       requester.email AS requester_email,
       COALESCE(assignee.name, '')         AS assignee_name,
       COALESCE(assignee.email::text, '')  AS assignee_email
FROM tickets t
JOIN users requester ON requester.id = t.requester_id
LEFT JOIN users assignee ON assignee.id = t.assignee_id
WHERE t.number = $1;

-- name: ListTickets :many
-- Filtered ticket list for the fixed views and ad-hoc combos:
--   statuses:   empty array = all statuses ("All open" passes the 3 open-ish ones)
--   priorities: empty array = all priorities
--   assignee_id: "My tickets"; unassigned=true: "Unassigned"
--   team_id / requester_id / tag_id: optional narrowing
-- total_count is the pre-LIMIT match count (window function), so one
-- round-trip serves both the page and the pager.
SELECT sqlc.embed(t),
       requester.name  AS requester_name,
       requester.email AS requester_email,
       COALESCE(assignee.name, '')         AS assignee_name,
       COALESCE(assignee.email::text, '')  AS assignee_email,
       count(*) OVER () AS total_count
FROM tickets t
JOIN users requester ON requester.id = t.requester_id
LEFT JOIN users assignee ON assignee.id = t.assignee_id
WHERE (COALESCE(cardinality(sqlc.arg('statuses')::text[]), 0) = 0 OR t.status::text = ANY (sqlc.arg('statuses')::text[]))
  AND (COALESCE(cardinality(sqlc.arg('priorities')::text[]), 0) = 0 OR t.priority::text = ANY (sqlc.arg('priorities')::text[]))
  AND (sqlc.narg('assignee_id')::uuid IS NULL OR t.assignee_id = sqlc.narg('assignee_id')::uuid)
  AND (NOT COALESCE(sqlc.narg('unassigned')::boolean, false) OR t.assignee_id IS NULL)
  AND (sqlc.narg('team_id')::uuid IS NULL OR t.team_id = sqlc.narg('team_id')::uuid)
  AND (sqlc.narg('requester_id')::uuid IS NULL OR t.requester_id = sqlc.narg('requester_id')::uuid)
  AND (sqlc.narg('tag_id')::uuid IS NULL OR EXISTS (
        SELECT 1 FROM ticket_tags tt
        WHERE tt.ticket_id = t.id AND tt.tag_id = sqlc.narg('tag_id')::uuid))
  -- closed_today mirrors the DashboardCounts closed-today predicate so the
  -- dashboard tile can deep-link a queue that matches its number.
  AND (NOT COALESCE(sqlc.narg('closed_today')::boolean, false)
       OR (t.status = 'closed' AND t.closed_at >= date_trunc('day', now())))
ORDER BY t.updated_at DESC, t.id
LIMIT sqlc.arg('page_limit')::bigint
OFFSET sqlc.arg('page_offset')::bigint;

-- name: SearchTickets :many
-- Full-text search across ticket subjects AND article bodies: the two
-- generated tsvector columns are matched separately, UNIONed, and ranked
-- per ticket by the best-matching hit. Same filters as ListTickets so
-- search composes with the fixed views.
WITH q AS (
    SELECT websearch_to_tsquery('english', sqlc.arg('query')::text) AS tsq
), hits AS (
    SELECT t.id AS ticket_id, ts_rank(t.search_tsv, q.tsq) AS rank
    FROM tickets t, q
    WHERE t.search_tsv @@ q.tsq
    UNION ALL
    SELECT a.ticket_id, ts_rank(a.search_tsv, q.tsq) AS rank
    FROM articles a, q
    WHERE a.search_tsv @@ q.tsq
), ranked AS (
    SELECT ticket_id, max(rank) AS rank
    FROM hits
    GROUP BY ticket_id
)
SELECT sqlc.embed(t),
       requester.name  AS requester_name,
       requester.email AS requester_email,
       COALESCE(assignee.name, '')         AS assignee_name,
       COALESCE(assignee.email::text, '')  AS assignee_email,
       ranked.rank::real AS rank,
       count(*) OVER () AS total_count
FROM ranked
JOIN tickets t ON t.id = ranked.ticket_id
JOIN users requester ON requester.id = t.requester_id
LEFT JOIN users assignee ON assignee.id = t.assignee_id
WHERE (COALESCE(cardinality(sqlc.arg('statuses')::text[]), 0) = 0 OR t.status::text = ANY (sqlc.arg('statuses')::text[]))
  AND (COALESCE(cardinality(sqlc.arg('priorities')::text[]), 0) = 0 OR t.priority::text = ANY (sqlc.arg('priorities')::text[]))
  AND (sqlc.narg('assignee_id')::uuid IS NULL OR t.assignee_id = sqlc.narg('assignee_id')::uuid)
  AND (NOT COALESCE(sqlc.narg('unassigned')::boolean, false) OR t.assignee_id IS NULL)
  AND (sqlc.narg('team_id')::uuid IS NULL OR t.team_id = sqlc.narg('team_id')::uuid)
  AND (sqlc.narg('requester_id')::uuid IS NULL OR t.requester_id = sqlc.narg('requester_id')::uuid)
  AND (sqlc.narg('tag_id')::uuid IS NULL OR EXISTS (
        SELECT 1 FROM ticket_tags tt
        WHERE tt.ticket_id = t.id AND tt.tag_id = sqlc.narg('tag_id')::uuid))
  -- closed_today mirrors the DashboardCounts closed-today predicate so the
  -- dashboard tile can deep-link a queue that matches its number.
  AND (NOT COALESCE(sqlc.narg('closed_today')::boolean, false)
       OR (t.status = 'closed' AND t.closed_at >= date_trunc('day', now())))
ORDER BY ranked.rank DESC, t.updated_at DESC, t.id
LIMIT sqlc.arg('page_limit')::bigint
OFFSET sqlc.arg('page_offset')::bigint;

-- name: UpdateTicketStatus :one
-- closed_at is stamped on the open->closed edge, preserved while closed,
-- and cleared on reopen.
UPDATE tickets
SET status     = sqlc.arg('status'),
    closed_at  = CASE
                     WHEN sqlc.arg('status')::ticket_status = 'closed' THEN COALESCE(tickets.closed_at, now())
                     ELSE NULL
                 END,
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: UpdateTicketPriority :one
UPDATE tickets
SET priority   = $2,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UpdateTicketAssignee :one
UPDATE tickets
SET assignee_id = $2,
    updated_at  = now()
WHERE id = $1
RETURNING *;

-- name: UpdateTicketTeam :one
UPDATE tickets
SET team_id    = $2,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: TouchTicket :one
-- Bump updated_at (e.g. when an article arrives) and return the row.
UPDATE tickets
SET updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DashboardCounts :one
-- The four dashboard counters in one scan. "Closed today" is measured in
-- the database's timezone (UTC in deployment).
SELECT count(*) FILTER (WHERE status = 'open')                                   AS open_count,
       count(*) FILTER (WHERE status <> 'closed' AND assignee_id IS NULL)        AS unassigned_count,
       count(*) FILTER (WHERE status = 'waiting_on_customer')                    AS waiting_on_customer_count,
       count(*) FILTER (WHERE status = 'closed' AND closed_at >= date_trunc('day', now())) AS closed_today_count
FROM tickets;
