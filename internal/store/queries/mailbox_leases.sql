-- Mailbox ownership leases (architecture doc §4: leader-elected lease per
-- mailbox; on worker death the lease lapses and another worker adopts).
-- All expiry math uses the database clock so N app nodes need no clock
-- agreement.

-- name: ClaimMailboxLease :one
-- Atomic claim-or-takeover. Succeeds (returns the lease) when the mailbox
-- is unleased, the existing lease has expired, or the caller already owns
-- it (re-claim extends). Otherwise no row is updated and the caller gets
-- pgx.ErrNoRows: somebody else holds a live lease.
INSERT INTO mailbox_leases (mailbox_id, owner, expires_at)
VALUES (
    sqlc.arg(mailbox_id),
    sqlc.arg(owner),
    now() + make_interval(secs => sqlc.arg(ttl_seconds)::float8)
)
ON CONFLICT (mailbox_id) DO UPDATE
SET owner      = EXCLUDED.owner,
    expires_at = EXCLUDED.expires_at
WHERE mailbox_leases.expires_at < now()
   OR mailbox_leases.owner = EXCLUDED.owner
RETURNING *;

-- name: RenewMailboxLease :one
-- Extend a lease we still hold. pgx.ErrNoRows means the lease expired and
-- was taken over (or released): the caller must stop polling this mailbox.
-- The owner check makes renewal safe even after a takeover; expiry is NOT
-- checked so a briefly-late renewal keeps the lease if nobody took it.
UPDATE mailbox_leases
SET expires_at = now() + make_interval(secs => sqlc.arg(ttl_seconds)::float8)
WHERE mailbox_id = sqlc.arg(mailbox_id)
  AND owner = sqlc.arg(owner)
RETURNING *;

-- name: ReleaseMailboxLease :execrows
-- Graceful shutdown: drop the lease so another worker can adopt the
-- mailbox immediately instead of waiting out the TTL. Owner-checked, so
-- releasing a lease lost to takeover is a harmless 0-row no-op.
DELETE FROM mailbox_leases
WHERE mailbox_id = sqlc.arg(mailbox_id)
  AND owner = sqlc.arg(owner);
