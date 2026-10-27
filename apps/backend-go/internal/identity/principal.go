package identity

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// Principal is the authenticated caller.
type Principal struct {
	User   User
	Claims auth.Claims
}

type ctxKey struct{}

// WithPrincipal stores p in ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFrom returns the authenticated principal, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

var bearer = httpx.Header{Key: "WWW-Authenticate", Value: "Bearer"}

// bearerToken mirrors OAuth2PasswordBearer: scheme "bearer" (any case) and a
// non-empty parameter, otherwise the caller is unauthenticated.
func bearerToken(r *http.Request) (string, bool) {
	scheme, param, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	param = strings.TrimSpace(param)
	if !strings.EqualFold(scheme, "bearer") || param == "" {
		return "", false
	}
	return param, true
}

// BearerToken is the exported bearer parser for resolvers that need their own
// status codes (chat auth) but must agree with Authenticate on the header.
func BearerToken(r *http.Request) (string, bool) { return bearerToken(r) }

// Authenticate validates the bearer access token, loads the user and stores a
// Principal in the request context. A workspace-scoped (SSO, sso_ws) token is
// refused (403) when the request names another workspace by X-Workspace-Id or
// by a {ws_id} / {workspace_id} path value; without the header, the header is
// set to the scope so every later workspace resolution sees it.
func Authenticate(signer auth.Signer, st *Store) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok, ok := bearerToken(r)
			if !ok {
				httpx.Detail(w, http.StatusUnauthorized, "Not authenticated", bearer)
				return
			}
			claims, err := signer.ParseAccess(tok)
			if err != nil {
				httpx.Detail(w, http.StatusUnauthorized, "Could not validate credentials", bearer)
				return
			}
			u, err := st.ByUsername(r.Context(), claims.Sub)
			if errors.Is(err, ErrNotFound) {
				httpx.Detail(w, http.StatusUnauthorized, "Could not validate credentials", bearer)
				return
			}
			if err != nil {
				httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
				return
			}
			if !u.IsActive {
				httpx.Detail(w, http.StatusBadRequest, "Inactive user")
				return
			}
			if !scopeAllowsRequest(r, claims) {
				httpx.Detail(w, http.StatusForbidden, workspaceDenied)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), Principal{User: u, Claims: claims})))
		})
	}
}

const workspaceDenied = "Workspace access denied"

// scopePathKeys are the route path values that name a workspace.
var scopePathKeys = []string{"ws_id", "workspace_id"}

// scopeAllowsRequest enforces a scoped token's workspace on what the request names up front and
// pins an absent X-Workspace-Id to the scope. Unscoped tokens pass untouched.
func scopeAllowsRequest(r *http.Request, c auth.Claims) bool {
	if c.Workspace == "" {
		return true
	}
	h := r.Header.Get("X-Workspace-Id")
	if h != "" && h != c.Workspace {
		return false
	}
	for _, k := range scopePathKeys {
		if v := r.PathValue(k); v != "" && v != c.Workspace {
			return false
		}
	}
	if h == "" {
		r.Header.Set("X-Workspace-Id", c.Workspace)
	}
	return true
}

// IsGlobalAdmin reports the legacy admin test: is_admin or role admin/superadmin.
func (u User) IsGlobalAdmin() bool {
	return u.IsAdmin || u.Role == "admin" || u.Role == "superadmin"
}

// RequireGlobalAdmin must run after Authenticate. A workspace-scoped (SSO)
// session is never a global admin session.
func RequireGlobalAdmin() httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok || !p.User.IsGlobalAdmin() || p.Claims.Workspace != "" {
				httpx.Detail(w, http.StatusForbidden, "Not authorized")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// QueryTokenError carries the HTTP status and detail of a failed query-token
// authentication.
type QueryTokenError struct {
	Status int
	Detail string
}

func (e *QueryTokenError) Error() string { return e.Detail }

// AuthenticateQueryToken authenticates a ?token= value (SSE/WebSocket). Unlike
// the header path, only a refresh type is rejected and an inactive user is 401.
func AuthenticateQueryToken(signer auth.Signer, st *Store, token string, requireAdmin bool) (Principal, error) {
	return AuthenticateQueryTokenCtx(context.Background(), signer, st, token, requireAdmin)
}

// AuthenticateQueryTokenCtx is AuthenticateQueryToken with a caller context. It
// does not know the workspace, so a workspace-scoped (SSO) token is refused as
// invalid (fail closed); use AuthenticateQueryTokenForWorkspace.
func AuthenticateQueryTokenCtx(ctx context.Context, signer auth.Signer, st *Store, token string, requireAdmin bool) (Principal, error) {
	return authenticateQueryToken(ctx, signer, st, token, requireAdmin, nil)
}

// AuthenticateQueryTokenForWorkspace is AuthenticateQueryTokenCtx for a caller
// that acts in workspaceID: a scoped token for another (or no) workspace is 403
// "Workspace access denied". Membership is still the caller's check.
func AuthenticateQueryTokenForWorkspace(ctx context.Context, signer auth.Signer, st *Store, token string, requireAdmin bool, workspaceID string) (Principal, error) {
	return authenticateQueryToken(ctx, signer, st, token, requireAdmin, &workspaceID)
}

func authenticateQueryToken(ctx context.Context, signer auth.Signer, st *Store, token string, requireAdmin bool, workspaceID *string) (Principal, error) {
	invalid := &QueryTokenError{http.StatusUnauthorized, "Invalid token"}
	if token == "" {
		return Principal{}, &QueryTokenError{http.StatusUnauthorized, "Missing token"}
	}
	claims, err := signer.ParseAccess(token)
	if err != nil {
		return Principal{}, invalid
	}
	if claims.Workspace != "" {
		if workspaceID == nil {
			return Principal{}, invalid
		}
		if !claims.AllowsWorkspace(*workspaceID) {
			return Principal{}, &QueryTokenError{http.StatusForbidden, workspaceDenied}
		}
	}
	u, err := st.ByUsername(ctx, claims.Sub)
	if errors.Is(err, ErrNotFound) || (err == nil && !u.IsActive) {
		return Principal{}, invalid
	}
	if err != nil {
		return Principal{}, err
	}
	if requireAdmin && !u.IsGlobalAdmin() {
		return Principal{}, &QueryTokenError{http.StatusForbidden, "Not authorized"}
	}
	return Principal{User: u, Claims: claims}, nil
}
