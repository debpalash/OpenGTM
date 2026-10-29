package identity

import (
	"context"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// RequireAdmin mirrors mcp_tokens.py:require_admin.
func RequireAdmin() httpx.Middleware { return RequireRole("secrets.manage", "admin") }

// RequireRole replicates require_workspace_role. It must run after Workspace.
// The owner always passes. With a permission, an explicit per-member allow or
// deny wins, else the role must be in roles; without one, the role must be in
// roles.
func RequireRole(permission string, roles ...string) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := WorkspaceFrom(r.Context())
			if !ok {
				httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
				return
			}
			if c.Role == "owner" {
				next.ServeHTTP(w, r)
				return
			}
			if permission == "" {
				if !slices.Contains(roles, c.Role) {
					httpx.Detail(w, http.StatusForbidden, "Insufficient workspace role")
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			allowed, err := hasPermission(r.Context(), c, permission, roles)
			if err != nil {
				httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
				return
			}
			if !allowed {
				httpx.Detail(w, http.StatusForbidden, "Workspace permission denied: "+permission)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func hasPermission(ctx context.Context, c WorkspaceCtx, permission string, roles []string) (bool, error) {
	var effect string
	var found bool
	err := c.Tx(ctx, func(tx pgx.Tx) error {
		rows, err := gen.New(tx).MemberPermissions(ctx, gen.MemberPermissionsParams{WorkspaceID: c.WorkspaceID, UserID: c.User.ID})
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Permission == permission {
				effect, found = row.Effect, true
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if found {
		return effect == "allow", nil
	}
	return slices.Contains(roles, c.Role), nil
}
