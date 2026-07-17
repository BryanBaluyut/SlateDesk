-- Teams and team membership: M1 CRUD.

-- name: GetTeamByID :one
SELECT * FROM teams
WHERE id = $1;

-- name: GetTeamByName :one
SELECT * FROM teams
WHERE name = $1;

-- name: ListTeams :many
SELECT * FROM teams
ORDER BY name;

-- name: CreateTeam :one
INSERT INTO teams (name, description)
VALUES ($1, $2)
RETURNING *;

-- name: UpdateTeam :one
UPDATE teams
SET name        = $2,
    description = $3
WHERE id = $1
RETURNING *;

-- name: DeleteTeam :exec
DELETE FROM teams
WHERE id = $1;

-- name: AddTeamMember :exec
INSERT INTO team_members (team_id, user_id)
VALUES ($1, $2)
ON CONFLICT (team_id, user_id) DO NOTHING;

-- name: RemoveTeamMember :exec
DELETE FROM team_members
WHERE team_id = $1
  AND user_id = $2;

-- name: RemoveAllTeamMembers :exec
DELETE FROM team_members
WHERE team_id = $1;

-- name: ListTeamMembers :many
SELECT u.* FROM users u
JOIN team_members tm ON tm.user_id = u.id
WHERE tm.team_id = $1
ORDER BY u.name, u.id;

-- name: ListTeamsForUser :many
SELECT t.* FROM teams t
JOIN team_members tm ON tm.team_id = t.id
WHERE tm.user_id = $1
ORDER BY t.name;
