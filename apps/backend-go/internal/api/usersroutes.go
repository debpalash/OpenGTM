package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

const maxUsersBody = 1 << 20

var validRoles = map[string]bool{"superadmin": true, "admin": true, "editor": true, "user": true, "viewer": true}

// RegisterUsers registers /admin/users (global admin only).
func RegisterUsers(r *httpx.Router, d Deps) {
	u := userRoutes{d}
	mws := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.RequireGlobalAdmin()}
	r.Handle("GET /admin/users", http.HandlerFunc(u.list), mws...)
	createMW := mws
	if d.Limiter != nil {
		// Account creation is throttled per authenticated admin (429 + Retry-After).
		createMW = append(append([]httpx.Middleware(nil), mws...), d.Limiter.Limit(userCreateRate))
	}
	r.Handle("POST /admin/users", http.HandlerFunc(u.create), createMW...)
	r.Handle("PUT /admin/users/{user_id}/password", http.HandlerFunc(u.resetPassword), mws...)
	r.Handle("DELETE /admin/users/{user_id}", http.HandlerFunc(u.remove), mws...)
	r.Handle("PATCH /admin/users/{user_id}/role", http.HandlerFunc(u.setRole), mws...)
}

type userRoutes struct{ d Deps }

func (u userRoutes) fail(w http.ResponseWriter, what string) {
	l := u.d.Log
	if l == nil {
		l = slog.Default()
	}
	l.Error("admin users: " + what + " failed")
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

// pathID parses {user_id} like pydantic's lax int; on failure it returns the 422 field error.
func pathID(r *http.Request) (int64, *httpx.FieldError) {
	raw := r.PathValue("user_id")
	s := strings.TrimSpace(raw)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil && errors.Is(err, strconv.ErrRange) {
		return -1, nil // out of range: no such user
	}
	if err != nil {
		return 0, &httpx.FieldError{Type: "int_parsing", Loc: []any{"path", "user_id"},
			Msg: "Input should be a valid integer, unable to parse string as an integer", Input: raw}
	}
	if n < 0 || n > 1<<31-1 {
		return -1, nil
	}
	return n, nil
}

// stringFields reads a JSON object body and extracts required string fields,
// returning pydantic-style errors for anything wrong.
func stringFields(w http.ResponseWriter, r *http.Request, pre []httpx.FieldError, names ...string) (map[string]string, bool) {
	errs := pre
	out := map[string]string{}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUsersBody))
	var body any
	if err == nil {
		err = json.Unmarshal(raw, &body)
	}
	if err != nil {
		errs = append(errs, httpx.FieldError{Type: "json_invalid", Loc: []any{"body", 0}, Msg: "JSON decode error", Input: map[string]any{}})
	} else if obj, isObj := body.(map[string]any); !isObj {
		errs = append(errs, httpx.FieldError{Type: "model_attributes_type", Loc: []any{"body"},
			Msg: "Input should be a valid dictionary or object to extract fields from", Input: body})
	} else {
		for _, n := range names {
			v, present := obj[n]
			s, isStr := v.(string)
			switch {
			case !present:
				errs = append(errs, httpx.FieldError{Type: "missing", Loc: []any{"body", n}, Msg: "Field required", Input: obj})
			case !isStr:
				errs = append(errs, httpx.FieldError{Type: "string_type", Loc: []any{"body", n}, Msg: "Input should be a valid string", Input: v})
			default:
				out[n] = s
			}
		}
	}
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return nil, false
	}
	return out, true
}

func (u userRoutes) list(w http.ResponseWriter, r *http.Request) {
	users, err := u.d.Identity.List(r.Context())
	if err != nil {
		u.fail(w, "list")
		return
	}
	if users == nil {
		users = []identity.User{}
	}
	httpx.WriteJSON(w, http.StatusOK, users)
}

func (u userRoutes) create(w http.ResponseWriter, r *http.Request) {
	f, ok := stringFields(w, r, nil, "username", "password")
	if !ok {
		return
	}
	ctx := r.Context()
	if _, err := u.d.Identity.ByUsername(ctx, f["username"]); err == nil {
		httpx.Detail(w, http.StatusBadRequest, "Username already registered")
		return
	} else if !errors.Is(err, identity.ErrNotFound) {
		u.fail(w, "lookup")
		return
	}
	hash, err := auth.HashPassword(f["password"])
	if err != nil {
		u.fail(w, "hash")
		return
	}
	nu, err := u.d.Identity.Insert(ctx, identity.NewUser{Username: f["username"], HashedPassword: hash, IsActive: true, Role: "user"})
	if err != nil {
		u.fail(w, "insert")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, nu)
}

func (u userRoutes) resetPassword(w http.ResponseWriter, r *http.Request) {
	id, perr := pathID(r)
	var pre []httpx.FieldError
	if perr != nil {
		pre = append(pre, *perr)
	}
	f, ok := stringFields(w, r, pre, "password")
	if !ok {
		return
	}
	if _, err := u.d.Identity.ByID(r.Context(), int32(id)); errors.Is(err, identity.ErrNotFound) {
		httpx.Detail(w, http.StatusNotFound, "User not found")
		return
	} else if err != nil {
		u.fail(w, "lookup")
		return
	}
	hash, err := auth.HashPassword(f["password"])
	if err != nil {
		u.fail(w, "hash")
		return
	}
	if err := u.d.Identity.UpdatePassword(r.Context(), int32(id), hash); err != nil {
		u.fail(w, "update")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"message": "Password updated successfully"})
}

func (u userRoutes) remove(w http.ResponseWriter, r *http.Request) {
	id, perr := pathID(r)
	if perr != nil {
		httpx.ValidationError(w, *perr)
		return
	}
	p, _ := identity.PrincipalFrom(r.Context())
	if id == int64(p.User.ID) {
		httpx.Detail(w, http.StatusBadRequest, "Cannot delete your own account")
		return
	}
	if _, err := u.d.Identity.ByID(r.Context(), int32(id)); errors.Is(err, identity.ErrNotFound) {
		httpx.Detail(w, http.StatusNotFound, "User not found")
		return
	} else if err != nil {
		u.fail(w, "lookup")
		return
	}
	if err := u.d.Identity.Delete(r.Context(), int32(id)); err != nil {
		u.fail(w, "delete")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"message": "User deleted"})
}

func (u userRoutes) setRole(w http.ResponseWriter, r *http.Request) {
	id, perr := pathID(r)
	var pre []httpx.FieldError
	if perr != nil {
		pre = append(pre, *perr)
	}
	f, ok := stringFields(w, r, pre, "role")
	if !ok {
		return
	}
	ctx := r.Context()
	if _, err := u.d.Identity.ByID(ctx, int32(id)); errors.Is(err, identity.ErrNotFound) {
		httpx.Detail(w, http.StatusNotFound, "User not found")
		return
	} else if err != nil {
		u.fail(w, "lookup")
		return
	}
	role := f["role"]
	if !validRoles[role] {
		httpx.Detail(w, http.StatusBadRequest, "Invalid role")
		return
	}
	if err := u.d.Identity.UpdateRole(ctx, int32(id), role, role == "admin"); err != nil {
		u.fail(w, "update")
		return
	}
	nu, err := u.d.Identity.ByID(ctx, int32(id))
	if err != nil {
		u.fail(w, "reload")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, nu)
}
