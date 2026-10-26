package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// WorkspaceCtx is the resolved tenant context for a request.
type WorkspaceCtx struct {
	User        User
	WorkspaceID string
	Slug        string
	Role        string // "" when the caller only owns the workspace (no member row)
	Claims      auth.Claims
	Pool        *pgxpool.Pool
}

// Tx runs fn in a transaction scoped to this workspace (RLS GUC set).
func (c WorkspaceCtx) Tx(ctx context.Context, fn func(pgx.Tx) error) error {
	return tenancy.InWorkspace(ctx, c.Pool, c.WorkspaceID, fn)
}

// Meta is the request-scoped actor information used for audit records.
type Meta struct {
	ActorUserID int32
	ActorRole   string
	WorkspaceID string
}

type wsKey struct{}
type metaKey struct{}

// WorkspaceFrom returns the resolved workspace context, if any.
func WorkspaceFrom(ctx context.Context) (WorkspaceCtx, bool) {
	c, ok := ctx.Value(wsKey{}).(WorkspaceCtx)
	return c, ok
}

// RequestMeta returns the audit actor set by Workspace (zero value if none).
func RequestMeta(ctx context.Context) Meta {
	m, _ := ctx.Value(metaKey{}).(Meta)
	return m
}

const ssoRequired = "This workspace requires SSO. Sign in with your organization identity."

// Workspace resolves and authorizes the caller's workspace. It must run after
// Authenticate. Order mirrors core/tenancy.py:current_workspace: header or
// active workspace, membership (identity plane, no GUC), slug, SSO. The RLS
// GUC is only set later by WorkspaceCtx.Tx.
func Workspace(st *Store) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			p, ok := PrincipalFrom(ctx)
			if !ok || st.pool == nil {
				httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
				return
			}
			fail := func(err error) {
				httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
			}
			uid := p.User.ID

			wsID := r.Header.Get("X-Workspace-Id")
			if wsID == "" {
				var err error
				if wsID, err = st.activeOrFirst(ctx, uid); err != nil {
					fail(err)
					return
				}
			}
			if wsID == "" {
				httpx.Detail(w, http.StatusForbidden, "No accessible workspace. Ask an admin to add you to one.")
				return
			}

			member, err := st.q.IsMember(ctx, gen.IsMemberParams{WorkspaceID: wsID, UserID: uid})
			if err != nil {
				fail(err)
				return
			}
			if !member {
				httpx.Detail(w, http.StatusForbidden, "Workspace access denied")
				return
			}

			ws, err := st.q.WorkspaceByID(ctx, wsID)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && ws.Slug == "") {
				httpx.Detail(w, http.StatusNotFound, "Workspace not found")
				return
			}
			if err != nil {
				fail(err)
				return
			}

			role, err := st.q.MemberRole(ctx, gen.MemberRoleParams{WorkspaceID: wsID, UserID: uid})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				fail(err)
				return
			}

			enforce, err := st.EnforceSSO(ctx, st.pool, wsID)
			if err != nil {
				fail(err)
				return
			}
			if enforce && role != "owner" && !hasAMR(p.Claims.AMR, "sso") {
				httpx.Detail(w, http.StatusForbidden, ssoRequired)
				return
			}

			wc := WorkspaceCtx{User: p.User, WorkspaceID: wsID, Slug: ws.Slug, Role: role, Claims: p.Claims, Pool: st.pool}
			ctx = context.WithValue(ctx, wsKey{}, wc)
			ctx = context.WithValue(ctx, metaKey{}, Meta{ActorUserID: uid, ActorRole: role, WorkspaceID: wsID})
			audit.SetActor(ctx, uid, role, wsID) // visible to the global audit middleware
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func hasAMR(amr []string, want string) bool {
	for _, a := range amr {
		if a == want {
			return true
		}
	}
	return false
}

// activeOrFirst returns the user's active workspace if they still belong to
// it, else the first accessible one, else "".
func (s *Store) activeOrFirst(ctx context.Context, uid int32) (string, error) {
	active, err := s.q.ActiveWorkspace(ctx, uid)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if err == nil && active != "" {
		ok, err := s.q.IsMember(ctx, gen.IsMemberParams{WorkspaceID: active, UserID: uid})
		if err != nil {
			return "", err
		}
		if ok {
			return active, nil
		}
	}
	first, err := s.q.FirstAccessibleWorkspace(ctx, uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return first, err
}

// EnforceSSO reads OIDC_CONFIG inside the workspace (FORCE RLS) and reports
// the merged enforce_sso value (DEFAULT_CONFIG has it false). The config is
// plaintext JSON; Python truthiness applies to the stored value.
func (s *Store) EnforceSSO(ctx context.Context, pool *pgxpool.Pool, workspaceID string) (bool, error) {
	var raw string
	err := tenancy.InWorkspace(ctx, pool, workspaceID, func(tx pgx.Tx) error {
		v, err := gen.New(tx).OIDCConfig(ctx, workspaceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		raw = v
		return err
	})
	if err != nil || raw == "" {
		return false, err
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return false, err
	}
	return truthy(cfg["enforce_sso"]), nil
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}
