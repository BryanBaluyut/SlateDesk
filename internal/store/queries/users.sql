-- Users: M1 CRUD.

-- name: GetUserByID :one
SELECT * FROM users
WHERE id = $1;

-- name: GetUserByEmail :one
SELECT * FROM users
WHERE email = $1;

-- name: ListUsers :many
-- q is a substring match on email/name; callers must escape ILIKE
-- wildcards (% _ \) in it first.
SELECT * FROM users
WHERE (sqlc.narg('role')::user_role IS NULL OR role = sqlc.narg('role')::user_role)
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active')::boolean)
  AND (sqlc.narg('q')::text IS NULL
       OR email ILIKE '%' || sqlc.narg('q')::text || '%'
       OR name ILIKE '%' || sqlc.narg('q')::text || '%')
ORDER BY created_at DESC, id
LIMIT sqlc.arg('page_limit')::bigint
OFFSET sqlc.arg('page_offset')::bigint;

-- name: CreateUser :one
INSERT INTO users (email, name, role, password_hash, company)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: UpdateUser :one
UPDATE users
SET name       = $2,
    role       = $3,
    company    = $4,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: SetUserPassword :exec
UPDATE users
SET password_hash = $2,
    updated_at    = now()
WHERE id = $1;

-- name: DeactivateUser :exec
UPDATE users
SET active     = false,
    updated_at = now()
WHERE id = $1;

-- name: ActivateUser :exec
UPDATE users
SET active     = true,
    updated_at = now()
WHERE id = $1;

-- name: EnsureUserByEmail :one
-- M3 inbound email: look up the sender by address (citext, so case folds),
-- creating a CUSTOMER on first contact. The conflict arm is a deliberate
-- no-op self-assignment: an existing user is returned unchanged — in
-- particular their role — so an unknown sender can NEVER be auto-created
-- as (or promoted to) agent/admin via email (architecture doc §4).
INSERT INTO users (email, name, role)
VALUES ($1, $2, 'customer')
ON CONFLICT (email) DO UPDATE
SET email = users.email
RETURNING *;

-- name: BumpUserTokenVersion :one
UPDATE users
SET token_version = token_version + 1,
    updated_at    = now()
WHERE id = $1
RETURNING token_version;

-- name: AdminExists :one
SELECT EXISTS (
    SELECT 1 FROM users
    WHERE role = 'admin' AND active
)::boolean AS admin_exists;

-- name: GetFirstAdmin :one
-- The earliest-created active admin, used at boot to attribute the seeded
-- welcome ticket on a headless/IaC install (where no wizard admin session
-- exists). Deterministic (created_at, then id) so it is stable across calls.
SELECT * FROM users
WHERE role = 'admin' AND active
ORDER BY created_at, id
LIMIT 1;

-- name: UpsertAdmin :one
INSERT INTO users (email, name, role, password_hash)
VALUES ($1, $2, 'admin', $3)
ON CONFLICT (email) DO UPDATE
SET name          = CASE WHEN EXCLUDED.name <> '' THEN EXCLUDED.name ELSE users.name END,
    role          = 'admin',
    password_hash = EXCLUDED.password_hash,
    active        = true,
    -- Resetting the password revokes every outstanding session, matching
    -- the PATCH /users password path (admin create is the recovery path).
    token_version = users.token_version + 1,
    updated_at    = now()
RETURNING *;
