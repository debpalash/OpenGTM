-- users is identity-plane (no RLS): run these on the plain pool.

-- name: UserByUsername :one
SELECT * FROM users WHERE username = sqlc.arg(username)::text;

-- name: UserByID :one
SELECT * FROM users WHERE id = sqlc.arg(id)::int;

-- name: ListUsers :many
SELECT * FROM users ORDER BY id;

-- name: InsertUser :one
INSERT INTO users (username, hashed_password, is_active, is_admin, role, profile_image, created_at)
VALUES (
  sqlc.arg(username)::text, sqlc.arg(hashed_password)::text, sqlc.arg(is_active)::bool,
  sqlc.arg(is_admin)::bool, sqlc.arg(role)::text, sqlc.narg(profile_image)::text,
  now() at time zone 'utc'
)
RETURNING *;

-- name: UpdateUserPassword :exec
UPDATE users SET hashed_password = sqlc.arg(hashed_password)::text WHERE id = sqlc.arg(id)::int;

-- name: UpdateUserRole :exec
UPDATE users SET role = sqlc.arg(role)::text, is_admin = sqlc.arg(is_admin)::bool WHERE id = sqlc.arg(id)::int;

-- name: DeleteUser :exec
DELETE FROM users WHERE id = sqlc.arg(id)::int;

-- name: TouchLastLogin :exec
UPDATE users SET last_login = now() at time zone 'utc' WHERE id = sqlc.arg(id)::int;
