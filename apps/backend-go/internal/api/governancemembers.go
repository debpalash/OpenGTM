package api

import (
	"net/http"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/govadmin"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const roleExpected = "'viewer', 'member', 'editor' or 'admin'"

func parseRole(obj *wbcrud.Obj, required bool) (string, *httpx.FieldError) {
	v, present := obj.Get("role")
	if !present {
		if required {
			return "", &httpx.FieldError{Type: "missing", Loc: []any{"body", "role"}, Msg: "Field required", Input: obj}
		}
		return "viewer", nil
	}
	if s, ok := v.(string); ok {
		for _, r := range govadmin.AssignableRoles {
			if s == r {
				return s, nil
			}
		}
	}
	e := literalErr("role", v, roleExpected)
	return "", &e
}

// parseMemberRoleUpdate: {"role": <enum>} with role required.
func parseMemberRoleUpdate(raw []byte) (string, []httpx.FieldError) {
	obj, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		return "", errs
	}
	role, e := parseRole(obj, true)
	if e != nil {
		return "", []httpx.FieldError{*e}
	}
	return role, nil
}

// parseMemberCreate: {"username": str 1..150 characters, "role": <enum> = "viewer"}. Both fields are validated and
// every error reported, in declaration order. The username is returned as sent (the domain trims it).
func parseMemberCreate(raw []byte) (string, string, []httpx.FieldError) {
	obj, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		return "", "", errs
	}
	var out []httpx.FieldError
	loc := []any{"body", "username"}
	username := ""
	switch v, present := obj.Get("username"); {
	case !present:
		out = append(out, httpx.FieldError{Type: "missing", Loc: loc, Msg: "Field required", Input: obj})
	default:
		s, ok := v.(string)
		switch n := utf8.RuneCountInString(s); {
		case !ok:
			out = append(out, httpx.FieldError{Type: "string_type", Loc: loc, Msg: "Input should be a valid string", Input: v})
		case n < 1:
			out = append(out, httpx.FieldError{Type: "string_too_short", Loc: loc, Msg: "String should have at least 1 character", Input: v, Ctx: map[string]any{"min_length": 1}})
		case n > 150:
			out = append(out, httpx.FieldError{Type: "string_too_long", Loc: loc, Msg: "String should have at most 150 characters", Input: v, Ctx: map[string]any{"max_length": 150}})
		default:
			username = s
		}
	}
	role, e := parseRole(obj, false)
	if e != nil {
		out = append(out, *e)
	}
	if out != nil {
		return "", "", out
	}
	return username, role, nil
}

func (h rbacRoutes) add(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	username, role, errs := parseMemberCreate(raw)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	var out govadmin.MemberOut
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) error {
		if err := govadmin.CheckAddMember(r.Context(), tx, c.WorkspaceID, c.User.ID, role); err != nil {
			return err
		}
		var err error
		out, err = govadmin.AddMember(r.Context(), tx, c.WorkspaceID, username, role)
		return err
	})
	h.finish(w, "add member", err, func() { httpx.WriteJSON(w, http.StatusCreated, out) })
}

func (h rbacRoutes) patch(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	uid, pathErr := h.pathUser(r)
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	role, errs := parseMemberRoleUpdate(raw)
	if pathErr != nil {
		errs = append([]httpx.FieldError{*pathErr}, errs...)
	}
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	var out govadmin.MemberOut
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) error {
		if err := govadmin.CheckSetRole(r.Context(), tx, c.WorkspaceID, c.User.ID, uid, role); err != nil {
			return err
		}
		var err error
		out, err = govadmin.SetRole(r.Context(), tx, c.WorkspaceID, uid, role)
		return err
	})
	h.finish(w, "set role", err, func() { httpx.WriteJSON(w, http.StatusOK, out) })
}

func (h rbacRoutes) remove(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	uid, pathErr := h.pathUser(r)
	if pathErr != nil {
		httpx.ValidationError(w, *pathErr)
		return
	}
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) error {
		if err := govadmin.CheckRemove(r.Context(), tx, c.WorkspaceID, c.User.ID, uid); err != nil {
			return err
		}
		return govadmin.Remove(r.Context(), tx, c.WorkspaceID, uid)
	})
	h.finish(w, "remove member", err, func() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNoContent)
	})
}
