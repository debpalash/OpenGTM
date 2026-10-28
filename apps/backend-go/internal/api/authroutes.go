package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

const maxAuthBody = 1 << 20

var wwwBearer = httpx.Header{Key: "WWW-Authenticate", Value: "Bearer"}

// RegisterAuth registers /auth/token, /auth/refresh, /auth/me, the workspace SSO login routes
// (/auth/sso/*, authsso.go) and the first-admin /auth/setup routes.
func RegisterAuth(r *httpx.Router, d Deps) {
	a := authRoutes{d: d}
	if d.Limiter != nil {
		perMin, perHour := d.Limiter.Keyed(tokenUserRate), d.Limiter.Keyed(tokenUserHourRate)
		a.userLimit = func(w http.ResponseWriter, key string) bool { return perMin(w, key) && perHour(w, key) }
	}
	var token http.Handler = http.HandlerFunc(a.token)
	var refresh http.Handler = http.HandlerFunc(a.refresh)
	if d.Limiter != nil {
		// Online password guessing (final security M2): per client IP (X-Real-IP only from a trusted proxy) on both
		// routes, and per username on /auth/token (userLimit, checked before the bcrypt verify). 429 carries Retry-After.
		token = d.Limiter.Limit(tokenIPRate)(token)
		refresh = d.Limiter.Limit(refreshIPRate)(refresh)
	}
	r.Handle("POST /auth/token", token)
	r.Handle("POST /auth/refresh", refresh)
	r.Handle("GET /auth/me", http.HandlerFunc(a.me), identity.Authenticate(d.Signer, d.Identity))
	registerSSOLogin(r, d)
	registerSetup(r, d)
}

// Login throttles: a small burst per window, then 429 until it reopens.
const (
	tokenIPBurst      = 20
	tokenUserBurst    = 5
	refreshIPBurst    = 60
	tokenIPRate       = "20/minute" // tokenIPBurst
	tokenUserRate     = "5/minute"  // tokenUserBurst
	tokenUserHourRate = "30/hour"
	refreshIPRate     = "60/minute" // refreshIPBurst
)

type authRoutes struct {
	d Deps
	// userLimit throttles /auth/token per normalised username (nil: no limiter wired).
	userLimit func(w http.ResponseWriter, key string) bool
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
}

func (a authRoutes) log() *slog.Logger {
	if a.d.Log != nil {
		return a.d.Log
	}
	return slog.Default()
}

func (a authRoutes) mint(w http.ResponseWriter, sub string, amr []string) (tokenResponse, bool) {
	return a.mintFor(w, sub, amr, "")
}

// mintFor issues an access + refresh pair scoped to the SSO workspace ws (sso_ws); "" is unscoped.
func (a authRoutes) mintFor(w http.ResponseWriter, sub string, amr []string, ws string) (tokenResponse, bool) {
	acc, err := a.d.Signer.AccessFor(sub, amr, ws)
	if err == nil {
		var ref string
		if ref, err = a.d.Signer.RefreshFor(sub, amr, ws); err == nil {
			return tokenResponse{acc, "bearer", ref}, true
		}
	}
	a.log().Error("auth: mint token failed")
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
	return tokenResponse{}, false
}

func formField(r *http.Request, name string) (string, bool) {
	v := r.PostForm.Get(name)
	return v, v != "" // FastAPI treats an empty required form value as missing
}

// dummyVerify is a seam so tests can observe the constant-time equalizer.
var dummyVerify = auth.DummyVerify

// noStore marks credential-bearing responses as uncacheable (RFC 6749 5.1).
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

func (a authRoutes) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBody)
	_ = r.ParseMultipartForm(maxAuthBody) // also parses urlencoded; other content types leave PostForm empty
	var errs []httpx.FieldError
	username, okU := formField(r, "username")
	password, okP := formField(r, "password")
	if !okU {
		errs = append(errs, httpx.FieldError{Type: "missing", Loc: []any{"body", "username"}, Msg: "Field required"})
	}
	if !okP {
		errs = append(errs, httpx.FieldError{Type: "missing", Loc: []any{"body", "password"}, Msg: "Field required"})
	}
	if gt := r.PostForm.Get("grant_type"); gt != "" && gt != "password" {
		errs = append(errs, httpx.FieldError{Type: "string_pattern_mismatch", Loc: []any{"body", "grant_type"},
			Msg: "String should match pattern '^password$'", Input: gt})
	}
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	if a.userLimit != nil && !a.userLimit(w, "login:"+strings.ToLower(strings.TrimSpace(username))) {
		return
	}
	bad := func() {
		httpx.Detail(w, http.StatusUnauthorized, "Incorrect username or password", wwwBearer)
	}
	u, hash, err := a.d.Identity.PasswordHashByUsername(r.Context(), username)
	if errors.Is(err, identity.ErrNotFound) {
		dummyVerify()
		bad()
		return
	}
	if err != nil {
		a.log().Error("auth: user lookup failed")
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if hash == "" {
		// SSO-only / no local password: spend the same time as a real verify.
		dummyVerify()
		bad()
		return
	}
	// The inactive check follows the real verify so a disabled account costs
	// the same time and answers the same 401 as a wrong password (S-L22).
	if !auth.VerifyPassword(password, hash) || !u.IsActive {
		bad()
		return
	}
	resp, ok := a.mint(w, u.Username, []string{"pwd"})
	if !ok {
		return
	}
	if err := a.d.Identity.TouchLastLogin(r.Context(), u.ID); err != nil {
		a.log().Error("auth: update last_login failed")
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	noStore(w)
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (a authRoutes) refresh(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAuthBody))
	if err != nil {
		httpx.ValidationError(w, httpx.FieldError{Type: "json_invalid", Loc: []any{"body", 0}, Msg: "JSON decode error", Input: map[string]any{}})
		return
	}
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		httpx.ValidationError(w, httpx.FieldError{Type: "json_invalid", Loc: []any{"body", 0}, Msg: "JSON decode error", Input: map[string]any{}})
		return
	}
	obj, isObj := body.(map[string]any)
	if !isObj {
		httpx.ValidationError(w, httpx.FieldError{Type: "model_attributes_type", Loc: []any{"body"},
			Msg: "Input should be a valid dictionary or object to extract fields from", Input: body})
		return
	}
	rt, present := obj["refresh_token"]
	if !present {
		httpx.ValidationError(w, httpx.FieldError{Type: "missing", Loc: []any{"body", "refresh_token"}, Msg: "Field required", Input: obj})
		return
	}
	tok, isStr := rt.(string)
	if !isStr {
		httpx.ValidationError(w, httpx.FieldError{Type: "string_type", Loc: []any{"body", "refresh_token"}, Msg: "Input should be a valid string", Input: rt})
		return
	}
	invalid := func() { httpx.Detail(w, http.StatusUnauthorized, "Invalid refresh token", wwwBearer) }
	claims, err := a.d.Signer.ParseAny(tok)
	if err != nil || claims.Type != "refresh" || claims.Sub == "" {
		invalid()
		return
	}
	u, err := a.d.Identity.ByUsername(r.Context(), claims.Sub)
	if errors.Is(err, identity.ErrNotFound) || (err == nil && !u.IsActive) {
		invalid()
		return
	}
	if err != nil {
		a.log().Error("auth: user lookup failed")
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	amr := claims.AMR
	if len(amr) == 0 {
		amr = []string{"pwd"}
	}
	// A workspace-scoped (SSO) session stays scoped across refreshes.
	if resp, ok := a.mintFor(w, claims.Sub, amr, claims.Workspace); ok {
		noStore(w)
		httpx.WriteJSON(w, http.StatusOK, resp)
	}
}

func (a authRoutes) me(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFrom(r.Context())
	httpx.WriteJSON(w, http.StatusOK, p.User)
}
