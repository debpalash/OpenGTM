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

// Authenticate validates the bearer access token, loads the user and stores a
// Principal in the request context.
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
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), Principal{User: u, Claims: claims})))
		})
	}
}

// IsGlobalAdmin reports the legacy admin test: is_admin or role admin/superadmin.
func (u User) IsGlobalAdmin() bool {
	return u.IsAdmin || u.Role == "admin" || u.Role == "superadmin"
}

// RequireGlobalAdmin must run after Authenticate.
func RequireGlobalAdmin() httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok || !p.User.IsGlobalAdmin() {
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

// AuthenticateQueryTokenCtx is AuthenticateQueryToken with a caller context.
func AuthenticateQueryTokenCtx(ctx context.Context, signer auth.Signer, st *Store, token string, requireAdmin bool) (Principal, error) {
	invalid := &QueryTokenError{http.StatusUnauthorized, "Invalid token"}
	if token == "" {
		return Principal{}, &QueryTokenError{http.StatusUnauthorized, "Missing token"}
	}
	claims, err := signer.ParseAccess(token)
	if err != nil {
		return Principal{}, invalid
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
