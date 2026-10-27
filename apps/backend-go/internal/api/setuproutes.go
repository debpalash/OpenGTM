package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

// First-run admin setup: the instance owner creates the admin account from the UI. There is no
// env-based admin. setup_complete (app_settings) blocks setup for good once a setup has run, even if
// every user is later deleted; there is deliberately no HTTP way back.
const (
	setupMarkerKey  = "setup_complete"
	setupRate       = "5/minute" // failed token attempts count too: the limiter wraps the whole handler
	minSetupPWLen   = 12
	minUsernameLen  = 3
	maxUsernameLen  = 64
	defaultWSName   = "Main"
	mainWorkspaceSl = "main"
)

// commonPasswords is a small deny list for the setup password (lower-case compare).
var commonPasswords = map[string]bool{
	"password1234": true, "123456789012": true, "qwertyuiop12": true, "administrator": true,
	"changeme1234": true, "letmein12345": true, "welcome12345": true, "password12345": true,
	"1234567890ab": true, "iloveyou1234": true, "opengtm12345": true, "passwordpassword": true,
	"adminadmin12": true, "admin1234567": true, "111111111111": true, "000000000000": true,
}

func registerSetup(r *httpx.Router, d Deps) {
	s := setupRoutes{authRoutes{d}}
	var post http.Handler = http.HandlerFunc(s.setup)
	var status http.Handler = http.HandlerFunc(s.status)
	if d.Limiter != nil {
		post = d.Limiter.Limit(setupRate)(post)
		status = d.Limiter.Limit("60/minute")(status)
	}
	r.Handle("GET /auth/setup/status", status)
	r.Handle("POST /auth/setup", post)
}

type setupRoutes struct{ authRoutes }

// setupRequired is true while no user exists and the completion marker is absent.
func setupRequired(ctx context.Context, q *gen.Queries) (bool, error) {
	hasUser, err := q.AnyUser(ctx)
	if err != nil || hasUser {
		return false, err
	}
	_, err = q.GetAppSetting(ctx, setupMarkerKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	return false, err
}

func (s setupRoutes) status(w http.ResponseWriter, r *http.Request) {
	req, err := setupRequired(r.Context(), gen.New(s.d.Pool))
	if err != nil {
		s.log().Error("auth: setup status failed")
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"setup_required": req, "token_required": req && s.d.SetupToken != ""})
}

type setupRequest struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	WorkspaceName string `json:"workspace_name"`
	SetupToken    string `json:"setup_token"`
}

func (s setupRoutes) setup(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAuthBody))
	var body map[string]json.RawMessage
	if err != nil || json.Unmarshal(raw, &body) != nil {
		httpx.ValidationError(w, httpx.FieldError{Type: "json_invalid", Loc: []any{"body", 0}, Msg: "JSON decode error"})
		return
	}
	// Strings only; any other JSON type for a field is a 422 on that field, never echoed back.
	var req setupRequest
	var errs []httpx.FieldError
	for _, f := range []struct {
		name     string
		dst      *string
		required bool
	}{{"username", &req.Username, true}, {"password", &req.Password, true}, {"workspace_name", &req.WorkspaceName, false}, {"setup_token", &req.SetupToken, false}} {
		v, present := body[f.name]
		if !present || string(v) == "null" {
			if f.required {
				errs = append(errs, httpx.FieldError{Type: "missing", Loc: []any{"body", f.name}, Msg: "Field required"})
			}
			continue
		}
		if json.Unmarshal(v, f.dst) != nil {
			errs = append(errs, httpx.FieldError{Type: "string_type", Loc: []any{"body", f.name}, Msg: "Input should be a valid string"})
		}
	}
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	// The token gate comes first so an unauthenticated caller learns nothing about the policy.
	if want := s.d.SetupToken; want != "" {
		if req.SetupToken == "" {
			httpx.Detail(w, http.StatusUnauthorized, "Setup token required", wwwBearer)
			return
		}
		if subtle.ConstantTimeCompare([]byte(req.SetupToken), []byte(want)) != 1 {
			httpx.Detail(w, http.StatusForbidden, "Invalid setup token")
			return
		}
	}
	req.Username = strings.TrimSpace(req.Username)
	if errs = validateSetup(req); len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	hash, err := auth.HashPassword(req.Password) // outside the lock: bcrypt cost 12 is slow
	if err != nil {
		s.log().Error("auth: setup hash failed")
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	u, done, err := s.create(r.Context(), req, hash)
	if err != nil {
		s.log().Error("auth: setup failed")
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if done {
		httpx.Detail(w, http.StatusConflict, "Setup already completed")
		return
	}
	resp, ok := s.mint(w, u.Username, []string{"pwd"})
	if !ok {
		return
	}
	if err := s.d.Identity.TouchLastLogin(r.Context(), u.ID); err != nil {
		s.log().Error("auth: update last_login failed")
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	s.log().Info("first admin created", "username", u.Username)
	noStore(w)
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func validateSetup(req setupRequest) []httpx.FieldError {
	var errs []httpx.FieldError
	bad := func(field, typ, msg string) {
		errs = append(errs, httpx.FieldError{Type: typ, Loc: []any{"body", field}, Msg: msg})
	}
	if n := utf8.RuneCountInString(req.Username); n < minUsernameLen || n > maxUsernameLen {
		bad("username", "string_length", "Username must be 3 to 64 characters")
	}
	switch {
	case utf8.RuneCountInString(req.Password) < minSetupPWLen:
		bad("password", "string_too_short", "Password must be at least 12 characters")
	case len(req.Password) > 72:
		bad("password", "string_too_long", "Password must be at most 72 bytes")
	case strings.EqualFold(req.Password, req.Username):
		bad("password", "value_error", "Password must differ from the username")
	case commonPasswords[strings.ToLower(req.Password)]:
		bad("password", "value_error", "Password is too common")
	}
	return errs
}

// create runs the whole setup in one transaction under an advisory lock, so concurrent calls
// serialize and all but the first see done=true.
func (s setupRoutes) create(ctx context.Context, req setupRequest, hash string) (u identity.User, done bool, err error) {
	tx, err := s.d.Pool.Begin(ctx)
	if err != nil {
		return u, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := gen.New(tx)
	if err = q.LockFirstAdminSetup(ctx); err != nil {
		return u, false, err
	}
	need, err := setupRequired(ctx, q)
	if err != nil || !need {
		return u, !need, err
	}
	row, err := q.InsertUser(ctx, gen.InsertUserParams{Username: req.Username, HashedPassword: hash, IsActive: true, IsAdmin: true, Role: "admin"})
	if err != nil {
		return u, false, err
	}
	now := nowEpoch()
	wsID, err := q.MainWorkspaceID(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Greenfield: create the default workspace like POST /api/workspaces does.
		if wsID, err = uuid4(); err != nil {
			return u, false, err
		}
		name := strings.TrimSpace(req.WorkspaceName)
		if name == "" {
			name = defaultWSName
		}
		if _, err = q.InsertWorkspace(ctx, gen.InsertWorkspaceParams{ID: wsID, Name: name, Slug: mainWorkspaceSl,
			Description: "", Icon: "🏢", OwnerID: row.ID, Now: now}); err != nil {
			return u, false, err
		}
	case err != nil:
		return u, false, err
	}
	// ensure_tenancy_backfill for migrated installs (a no-op on greenfield): ownerless workspaces
	// get this owner and the admin is an owner member of every workspace.
	if err = q.ClaimOwnerlessWorkspaces(ctx, row.ID); err != nil {
		return u, false, err
	}
	if err = q.AddOwnerToAllWorkspaces(ctx, gen.AddOwnerToAllWorkspacesParams{UserID: row.ID, Now: now}); err != nil {
		return u, false, err
	}
	if err = q.SetActiveWorkspace(ctx, gen.SetActiveWorkspaceParams{UserID: row.ID, WorkspaceID: wsID}); err != nil {
		return u, false, err
	}
	if err = q.UpsertAppSetting(ctx, gen.UpsertAppSettingParams{Key: setupMarkerKey, Value: "1"}); err != nil {
		return u, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return u, false, err
	}
	u, err = s.d.Identity.ByID(ctx, row.ID)
	return u, false, err
}

// ResolveSetupToken decides the first-admin setup token once at API start. SETUP_TOKEN wins; dev/test
// APP_ENV or SETUP_TOKEN_DISABLED=1 means no token; otherwise, only while setup is still required, a
// random token is generated and logged once. Empty result: no token needed. A configured token is
// never logged.
func ResolveSetupToken(ctx context.Context, pool *pgxpool.Pool, getenv func(string) string, appEnv string, log *slog.Logger) (string, error) {
	if t := getenv("SETUP_TOKEN"); t != "" {
		return t, nil
	}
	if v := strings.TrimSpace(getenv("SETUP_TOKEN_DISABLED")); v == "1" || strings.EqualFold(v, "true") {
		return "", nil
	}
	switch strings.ToLower(strings.TrimSpace(appEnv)) {
	case "dev", "development", "test", "local":
		return "", nil
	}
	need, err := setupRequired(ctx, gen.New(pool))
	if err != nil || !need {
		return "", err
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	log.Warn("first-run setup is open: open the UI and enter this setup token to create the admin account (shown once)", "setup_token", tok)
	return tok, nil
}
