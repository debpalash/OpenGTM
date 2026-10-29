package govadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// RBACMember is one /rbac member row (list_members + username + overrides).
type RBACMember struct {
	UserID    int32             `json:"user_id"`
	Role      string            `json:"role"`
	CreatedAt *float64          `json:"created_at"`
	Username  string            `json:"username"`
	Overrides map[string]string `json:"overrides"`
}

// RBAC is the GET /rbac body.
type RBAC struct {
	Permissions []Permission `json:"permissions"`
	Members     []RBACMember `json:"members"`
}

// Effective is the PUT /rbac/{user}/{permission} body.
type Effective struct {
	UserID     int32   `json:"user_id"`
	Permission string  `json:"permission"`
	Effect     *string `json:"effect"`
	Effective  bool    `json:"effective"`
}

// MemberOut is the POST (username set) and PATCH (username empty, omitted) body.
type MemberOut struct {
	UserID    int32             `json:"user_id"`
	Username  string            `json:"username,omitempty"`
	Role      string            `json:"role"`
	Overrides map[string]string `json:"overrides"`
}

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func f8(v pgtype.Float8) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}

// Overview is GET /rbac. tx must be inside tenancy.InWorkspace(ws).
func Overview(ctx context.Context, tx pgx.Tx, ws string) (RBAC, error) {
	q := gen.New(tx)
	rows, err := q.GovListMembers(ctx, ws)
	if err != nil {
		return RBAC{}, err
	}
	ids := make([]int32, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.UserID)
	}
	names := map[int32]string{}
	if len(ids) > 0 {
		us, err := q.GovUsernames(ctx, ids)
		if err != nil {
			return RBAC{}, err
		}
		for _, u := range us {
			if u.Username.Valid {
				names[u.ID] = u.Username.String
			}
		}
	}
	ovs, err := q.GovListOverrides(ctx, ws)
	if err != nil {
		return RBAC{}, err
	}
	byUser := map[int32]map[string]string{}
	for _, o := range ovs {
		if byUser[o.UserID] == nil {
			byUser[o.UserID] = map[string]string{}
		}
		byUser[o.UserID][o.Permission] = o.Effect
	}
	out := RBAC{Permissions: slices.Clone(Permissions), Members: make([]RBACMember, 0, len(rows))}
	for _, r := range rows {
		name, ok := names[r.UserID]
		if !ok {
			name = fmt.Sprintf("User %d", r.UserID)
		}
		ov := byUser[r.UserID]
		if ov == nil {
			ov = map[string]string{}
		}
		out.Members = append(out.Members, RBACMember{UserID: r.UserID, Role: r.Role, CreatedAt: f8(r.CreatedAt), Username: name, Overrides: ov})
	}
	return out, nil
}

func lockRole(ctx context.Context, q *gen.Queries, ws string, uid int32) (string, error) {
	role, err := q.GovLockMemberRole(ctx, gen.GovLockMemberRoleParams{WorkspaceID: ws, UserID: uid})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotMember
	}
	return role, err
}

func overridesOf(ctx context.Context, q *gen.Queries, ws string, uid int32) (map[string]string, error) {
	rows, err := q.MemberPermissions(ctx, gen.MemberPermissionsParams{WorkspaceID: ws, UserID: uid})
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, r := range rows {
		m[r.Permission] = r.Effect
	}
	return m, nil
}

// SetOverride is PUT /rbac/{user}/{permission}. Order matches Python: unknown
// permission, then member lookup, then owner. effect nil clears the override.
func SetOverride(ctx context.Context, tx pgx.Tx, ws string, userID int32, permission string, effect *string) (Effective, error) {
	perm, ok := lookup(permission)
	if !ok {
		return Effective{}, ErrUnknownPermission
	}
	if effect != nil && *effect != "allow" && *effect != "deny" {
		return Effective{}, ErrInvalidEffect
	}
	q := gen.New(tx)
	role, err := lockRole(ctx, q, ws, userID)
	if err != nil {
		return Effective{}, err
	}
	if role == "owner" {
		return Effective{}, ErrOwner
	}
	if effect == nil {
		err = q.GovDeleteOverride(ctx, gen.GovDeleteOverrideParams{WorkspaceID: ws, UserID: userID, Permission: permission})
	} else {
		err = q.GovUpsertOverride(ctx, gen.GovUpsertOverrideParams{WorkspaceID: ws, UserID: userID, Permission: permission, Effect: *effect, Now: now()})
	}
	if err != nil {
		return Effective{}, err
	}
	eff := (effect != nil && *effect == "allow") || (effect == nil && slices.Contains(perm.DefaultRoles, role))
	return Effective{UserID: userID, Permission: permission, Effect: effect, Effective: eff}, nil
}

// AddMember is POST /members. The username is trimmed; owner is not assignable.
func AddMember(ctx context.Context, tx pgx.Tx, ws, username, role string) (MemberOut, error) {
	if !validRole(role) {
		return MemberOut{}, ErrInvalidRole
	}
	q := gen.New(tx)
	u, err := q.GovUserByUsername(ctx, strings.TrimSpace(username))
	if errors.Is(err, pgx.ErrNoRows) {
		return MemberOut{}, ErrUserNotFound
	}
	if err != nil {
		return MemberOut{}, err
	}
	is, err := q.IsMember(ctx, gen.IsMemberParams{WorkspaceID: ws, UserID: u.ID})
	if err != nil {
		return MemberOut{}, err
	}
	if is {
		return MemberOut{}, ErrAlreadyMember
	}
	n, err := q.GovInsertMember(ctx, gen.GovInsertMemberParams{WorkspaceID: ws, UserID: u.ID, Role: role, Now: now()})
	if err != nil {
		return MemberOut{}, err
	}
	if n == 0 { // lost a concurrent add
		return MemberOut{}, ErrAlreadyMember
	}
	return MemberOut{UserID: u.ID, Username: u.Username.String, Role: role, Overrides: map[string]string{}}, nil
}

// SetRole is PATCH /members/{id}. Overrides are preserved.
func SetRole(ctx context.Context, tx pgx.Tx, ws string, uid int32, role string) (MemberOut, error) {
	if !validRole(role) {
		return MemberOut{}, ErrInvalidRole
	}
	q := gen.New(tx)
	cur, err := lockRole(ctx, q, ws, uid)
	if err != nil {
		return MemberOut{}, err
	}
	if cur == "owner" {
		return MemberOut{}, ErrOwnerRole
	}
	if _, err := q.GovUpdateMemberRole(ctx, gen.GovUpdateMemberRoleParams{WorkspaceID: ws, UserID: uid, Role: role}); err != nil {
		return MemberOut{}, err
	}
	ov, err := overridesOf(ctx, q, ws, uid)
	if err != nil {
		return MemberOut{}, err
	}
	return MemberOut{UserID: uid, Role: role, Overrides: ov}, nil
}

// Remove is DELETE /members/{id}: OIDC identities, overrides, SCIM group
// memberships (deviation D6) and the member row, in the caller's transaction.
func Remove(ctx context.Context, tx pgx.Tx, ws string, uid int32) error {
	q := gen.New(tx)
	role, err := lockRole(ctx, q, ws, uid)
	if err != nil {
		return err
	}
	if role == "owner" {
		return ErrOwnerRemove
	}
	steps := []func() error{
		func() error {
			return q.GovDeleteMemberOIDC(ctx, gen.GovDeleteMemberOIDCParams{WorkspaceID: ws, UserID: uid})
		},
		func() error {
			return q.GovDeleteMemberOverrides(ctx, gen.GovDeleteMemberOverridesParams{WorkspaceID: ws, UserID: uid})
		},
		func() error {
			return q.GovDeleteMemberSCIMGroups(ctx, gen.GovDeleteMemberSCIMGroupsParams{WorkspaceID: ws, UserID: uid})
		},
		func() error { return q.GovDeleteMember(ctx, gen.GovDeleteMemberParams{WorkspaceID: ws, UserID: uid}) },
	}
	for _, s := range steps {
		if err := s(); err != nil {
			return err
		}
	}
	return nil
}

// pyFloat marshals like Python's json.dumps(float): shortest round-trip digits, ".0" on integral values.
type pyFloat float64

func (f pyFloat) MarshalJSON() ([]byte, error) {
	v := float64(f)
	if a := math.Abs(v); a != 0 && (a < 1e-4 || a >= 1e16) {
		return json.Marshal(v) // exponent forms differ from Python in rare ranges; epoch seconds never reach them
	}
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return []byte(s), nil
}

// MarshalJSON renders created_at as a Python float (1788000001.0), key order as declared.
func (m RBACMember) MarshalJSON() ([]byte, error) {
	var created *pyFloat
	if m.CreatedAt != nil {
		c := pyFloat(*m.CreatedAt)
		created = &c
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // usernames reach the wire verbatim, as Python's json.dumps
	err := enc.Encode(struct {
		UserID    int32             `json:"user_id"`
		Role      string            `json:"role"`
		CreatedAt *pyFloat          `json:"created_at"`
		Username  string            `json:"username"`
		Overrides map[string]string `json:"overrides"`
	}{m.UserID, m.Role, created, m.Username, m.Overrides})
	return bytes.TrimRight(buf.Bytes(), "\n"), err
}
