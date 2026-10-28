package api

import (
	"errors"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/govadmin"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// maxRBACBody bounds the member and override bodies (Python has no limit; Go answers 413).
const maxRBACBody = 1 << 20

type rbacRoutes struct{ d Deps }

// Member-add throttle (per workspace) and /admin/users create throttle (per admin user).
const (
	memberAddBurst  = 20
	memberAddRate   = "20/minute" // memberAddBurst
	userCreateBurst = 10
	userCreateRate  = "10/minute" // userCreateBurst
)

// registerGovernanceRBAC registers the rbac and members routes (Task 14). All need governance.manage (admin),
// as Python's require_admin.
func registerGovernanceRBAC(r *httpx.Router, d Deps) {
	registerRBACRoutes(r, d, []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity), govadmin.Gate("governance.manage")})
}

// registerRBACRoutes takes the gate chain as a parameter so a test can model a gate decision that went stale
// before the write (the handlers re-check inside the write transaction).
func registerRBACRoutes(r *httpx.Router, d Deps, mw []httpx.Middleware) {
	h := rbacRoutes{d: d}
	// Adding an account to a workspace is the invite path of this API: throttled per workspace
	// (the limiter keys on the authenticated workspace), 429 + Retry-After, after the gate chain.
	addMW := mw
	if d.Limiter != nil {
		addMW = append(append([]httpx.Middleware(nil), mw...), d.Limiter.Limit(memberAddRate))
	}
	r.Handle("GET /api/governance/rbac", http.HandlerFunc(h.get), mw...)
	r.Handle("PUT /api/governance/rbac/{user_id}/{permission}", http.HandlerFunc(h.put), mw...)
	r.Handle("POST /api/governance/members", http.HandlerFunc(h.add), addMW...)
	r.Handle("PATCH /api/governance/members/{user_id}", http.HandlerFunc(h.patch), mw...)
	r.Handle("DELETE /api/governance/members/{user_id}", http.HandlerFunc(h.remove), mw...)
}

// --- request bodies (PermissionUpdate, MemberCreate, MemberRoleUpdate) ---------------------------------------

const (
	effectExpected = "'allow' or 'deny'"
)

func literalErr(field string, input any, expected string) httpx.FieldError {
	return httpx.FieldError{Type: "literal_error", Loc: []any{"body", field}, Msg: "Input should be " + expected,
		Input: input, Ctx: map[string]any{"expected": expected}}
}

// parsePermissionUpdate: {"effect": "allow"|"deny"|null}; absent means null (clear the override).
func parsePermissionUpdate(raw []byte) (*string, []httpx.FieldError) {
	obj, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		return nil, errs
	}
	v, _ := obj.Get("effect")
	if v == nil {
		return nil, nil
	}
	if s, ok := v.(string); ok && (s == "allow" || s == "deny") {
		return &s, nil
	}
	return nil, []httpx.FieldError{literalErr("effect", v, effectExpected)}
}

// --- handlers ------------------------------------------------------------------------------------------------

func (h rbacRoutes) ctx(w http.ResponseWriter, r *http.Request) (identity.WorkspaceCtx, bool) {
	c, ok := identity.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
	}
	return c, ok
}

func (h rbacRoutes) body(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRBACBody))
	var tooBig *http.MaxBytesError
	switch {
	case err == nil:
		return raw, true
	case errors.As(err, &tooBig):
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
	default: // client abort or a broken stream: FastAPI answers 400
		httpx.Detail(w, http.StatusBadRequest, "There was an error parsing the body")
	}
	return nil, false
}

// pathUser parses {user_id}; an id Postgres integer cannot hold matches no member (Python answers 500).
func (h rbacRoutes) pathUser(r *http.Request) (int32, *httpx.FieldError) {
	n, fe := pathID(r)
	if fe != nil {
		return 0, fe
	}
	return int32(n), nil // -1 for out of range: no such member
}

// fail never logs request data; only the error type.
func (h rbacRoutes) fail(w http.ResponseWriter, op string, err error) {
	h.d.logger().Error("governance rbac: request failed", "op", op, "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

// mapErr answers a domain error with Python's status and detail; false means it is not a domain error.
func (h rbacRoutes) mapErr(w http.ResponseWriter, err error) bool {
	status := 0
	switch {
	case errors.Is(err, govadmin.ErrForbidden), errors.Is(err, govadmin.ErrEscalation):
		status = http.StatusForbidden
	case errors.Is(err, govadmin.ErrUnknownPermission), errors.Is(err, govadmin.ErrNotMember), errors.Is(err, govadmin.ErrUserNotFound):
		status = http.StatusNotFound
	case errors.Is(err, govadmin.ErrOwner), errors.Is(err, govadmin.ErrOwnerRole), errors.Is(err, govadmin.ErrOwnerRemove), errors.Is(err, govadmin.ErrAlreadyMember):
		status = http.StatusConflict
	}
	if status == 0 {
		return false
	}
	httpx.Detail(w, status, err.Error())
	return true
}

func (h rbacRoutes) get(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	var out govadmin.RBAC
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) (err error) {
		out, err = govadmin.Overview(r.Context(), tx, c.WorkspaceID)
		return err
	})
	if err != nil {
		h.fail(w, "overview", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// finish writes the result of a write transaction: success via ok, a domain error with its status, else a 500.
func (h rbacRoutes) finish(w http.ResponseWriter, op string, err error, ok func()) {
	switch {
	case err == nil:
		ok()
	case h.mapErr(w, err):
	default:
		h.fail(w, op, err)
	}
}

func (h rbacRoutes) put(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	uid, pathErr := h.pathUser(r)
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	effect, errs := parsePermissionUpdate(raw)
	if pathErr != nil {
		errs = append([]httpx.FieldError{*pathErr}, errs...)
	}
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	perm := r.PathValue("permission")
	var out govadmin.Effective
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) error {
		if err := govadmin.CheckSetOverride(r.Context(), tx, c.WorkspaceID, c.User.ID, uid, perm, effect); err != nil {
			return err
		}
		var err error
		out, err = govadmin.SetOverride(r.Context(), tx, c.WorkspaceID, uid, perm, effect)
		return err
	})
	h.finish(w, "set override", err, func() { httpx.WriteJSON(w, http.StatusOK, out) })
}
