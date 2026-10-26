// Package identity holds the identity-plane user store and the bearer
// authentication middleware. The users table has no RLS: use the plain pool.
package identity

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// ErrNotFound is returned when no user matches.
var ErrNotFound = errors.New("user not found")

// User is the API view of a users row. The password hash is never carried.
type User struct {
	ID           int32
	Username     string
	IsActive     bool
	IsAdmin      bool
	Role         string
	ProfileImage *string
	LastLogin    *time.Time
	CreatedAt    *time.Time
}

// naive formats like pydantic for naive datetimes: no zone, microsecond
// fraction only when non-zero.
func naive(t *time.Time) *string {
	if t == nil {
		return nil
	}
	u := t.UTC()
	s := u.Format("2006-01-02T15:04:05")
	if us := u.Nanosecond() / 1000; us != 0 {
		s += u.Format(".000000")
	}
	return &s
}

// MarshalJSON produces the FastAPI UserResponse shape.
func (u User) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID           int32   `json:"id"`
		Username     string  `json:"username"`
		IsActive     bool    `json:"is_active"`
		IsAdmin      bool    `json:"is_admin"`
		Role         string  `json:"role"`
		ProfileImage *string `json:"profile_image"`
		LastLogin    *string `json:"last_login"`
		CreatedAt    *string `json:"created_at"`
	}{u.ID, u.Username, u.IsActive, u.IsAdmin, u.Role, u.ProfileImage, naive(u.LastLogin), naive(u.CreatedAt)})
}

func ts(t pgtype.Timestamp) *time.Time {
	if !t.Valid || t.InfinityModifier != pgtype.Finite {
		return nil
	}
	v := t.Time
	return &v
}

func fromRow(r gen.User) User {
	u := User{
		ID:        r.ID,
		Username:  r.Username.String,
		IsActive:  r.IsActive.Valid && r.IsActive.Bool,
		IsAdmin:   r.IsAdmin.Valid && r.IsAdmin.Bool,
		Role:      r.Role.String,
		LastLogin: ts(r.LastLogin),
		CreatedAt: ts(r.CreatedAt),
	}
	if r.ProfileImage.Valid {
		s := r.ProfileImage.String
		u.ProfileImage = &s
	}
	return u
}

// Store reads and writes users on the plain (identity-plane) pool.
type Store struct {
	q    *gen.Queries
	pool *pgxpool.Pool // set when built from a pool; needed by Workspace
}

// NewStore wraps a plain pool or connection.
func NewStore(db gen.DBTX) *Store {
	s := &Store{q: gen.New(db)}
	s.pool, _ = db.(*pgxpool.Pool)
	return s
}

func wrap(r gen.User, err error) (User, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	return fromRow(r), nil
}

// ByUsername looks a user up by exact username.
func (s *Store) ByUsername(ctx context.Context, name string) (User, error) {
	return wrap(s.q.UserByUsername(ctx, name))
}

// ByID looks a user up by id.
func (s *Store) ByID(ctx context.Context, id int32) (User, error) {
	return wrap(s.q.UserByID(ctx, id))
}

// PasswordHashByUsername returns the user and stored hash for login checks.
func (s *Store) PasswordHashByUsername(ctx context.Context, name string) (User, string, error) {
	r, err := s.q.UserByUsername(ctx, name)
	u, err := wrap(r, err)
	return u, r.HashedPassword.String, err
}

// List returns all users ordered by id.
func (s *Store) List(ctx context.Context) ([]User, error) {
	rows, err := s.q.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]User, len(rows))
	for i, r := range rows {
		out[i] = fromRow(r)
	}
	return out, nil
}

// NewUser is the input to Insert.
type NewUser struct {
	Username       string
	HashedPassword string
	IsActive       bool
	IsAdmin        bool
	Role           string
	ProfileImage   *string
}

// Insert creates a user; created_at is set to UTC now by the database.
func (s *Store) Insert(ctx context.Context, n NewUser) (User, error) {
	p := gen.InsertUserParams{
		Username: n.Username, HashedPassword: n.HashedPassword,
		IsActive: n.IsActive, IsAdmin: n.IsAdmin, Role: n.Role,
	}
	if n.ProfileImage != nil {
		p.ProfileImage = pgtype.Text{String: *n.ProfileImage, Valid: true}
	}
	return wrap(s.q.InsertUser(ctx, p))
}

// UpdatePassword stores a new password hash.
func (s *Store) UpdatePassword(ctx context.Context, id int32, hash string) error {
	return s.q.UpdateUserPassword(ctx, gen.UpdateUserPasswordParams{ID: id, HashedPassword: hash})
}

// UpdateRole sets role and is_admin together.
func (s *Store) UpdateRole(ctx context.Context, id int32, role string, isAdmin bool) error {
	return s.q.UpdateUserRole(ctx, gen.UpdateUserRoleParams{ID: id, Role: role, IsAdmin: isAdmin})
}

// Delete removes a user.
func (s *Store) Delete(ctx context.Context, id int32) error { return s.q.DeleteUser(ctx, id) }

// TouchLastLogin sets last_login to UTC now.
func (s *Store) TouchLastLogin(ctx context.Context, id int32) error {
	return s.q.TouchLastLogin(ctx, id)
}
