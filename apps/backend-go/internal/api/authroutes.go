package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

const maxAuthBody = 1 << 20

var wwwBearer = httpx.Header{Key: "WWW-Authenticate", Value: "Bearer"}

// RegisterAuth registers /auth/token, /auth/refresh and /auth/me.
func RegisterAuth(r *httpx.Router, d Deps) {
	a := authRoutes{d}
	r.Handle("POST /auth/token", http.HandlerFunc(a.token))
	r.Handle("POST /auth/refresh", http.HandlerFunc(a.refresh))
	r.Handle("GET /auth/me", http.HandlerFunc(a.me), identity.Authenticate(d.Signer, d.Identity))
}

type authRoutes struct{ d Deps }

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
	acc, err := a.d.Signer.Access(sub, amr)
	if err == nil {
		var ref string
		if ref, err = a.d.Signer.Refresh(sub, amr); err == nil {
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
	bad := func() {
		httpx.Detail(w, http.StatusUnauthorized, "Incorrect username or password", wwwBearer)
	}
	u, hash, err := a.d.Identity.PasswordHashByUsername(r.Context(), username)
	if errors.Is(err, identity.ErrNotFound) {
		auth.DummyVerify()
		bad()
		return
	}
	if err != nil {
		a.log().Error("auth: user lookup failed")
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if !auth.VerifyPassword(password, hash) {
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
	if resp, ok := a.mint(w, claims.Sub, amr); ok {
		httpx.WriteJSON(w, http.StatusOK, resp)
	}
}

func (a authRoutes) me(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFrom(r.Context())
	httpx.WriteJSON(w, http.StatusOK, p.User)
}
