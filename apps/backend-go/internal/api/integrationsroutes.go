package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
)

// integrationRoutes serves /api/settings/integrations (global) and
// /api/settings/workspace-integrations (per workspace). The workspace routes
// take ?workspace_id= and replicate routers/settings.py's explicit checks, so
// they do not use the X-Workspace-Id middleware; resolveWorkspace applies the
// same session scope and SSO enforcement that middleware does (final review M1).
type integrationRoutes struct {
	settingsRoutes
	integ *settings.Integrations
	st    *identity.Store
	pool  *pgxpool.Pool
}

func (s integrationRoutes) list(w http.ResponseWriter, r *http.Request) {
	views, err := s.integ.GlobalViews(r.Context())
	if err != nil {
		s.fail(w, "integrations list")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"integrations": views})
}

// parseIntegrationUpdate validates {"values": Dict[str, str]} like pydantic
// and returns the entries in request order (a duplicate key keeps its first
// position and takes the last value, as a Python dict does).
func parseIntegrationUpdate(raw []byte) ([]settings.KV, []httpx.FieldError) {
	obj, errs := decodeBodyObject(raw)
	if errs != nil {
		return nil, errs
	}
	v, present := obj["values"]
	if !present {
		return nil, []httpx.FieldError{{Type: "missing", Loc: []any{"body", "values"}, Msg: "Field required", Input: obj}}
	}
	if _, isObj := v.(map[string]any); !isObj {
		return nil, []httpx.FieldError{{Type: "dict_type", Loc: []any{"body", "values"}, Msg: "Input should be a valid dictionary", Input: v}}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, []httpx.FieldError{{Type: "json_invalid", Loc: []any{"body", 0}, Msg: "JSON decode error", Input: map[string]any{}}}
	}
	dec := json.NewDecoder(bytes.NewReader(top["values"]))
	dec.UseNumber()
	_, _ = dec.Token() // {
	var out []settings.KV
	index := map[string]int{}
	for dec.More() {
		kt, _ := dec.Token()
		key, _ := kt.(string)
		var val any
		if err := dec.Decode(&val); err != nil {
			return nil, []httpx.FieldError{{Type: "json_invalid", Loc: []any{"body", 0}, Msg: "JSON decode error", Input: map[string]any{}}}
		}
		str, isStr := val.(string)
		if !isStr {
			errs = append(errs, httpx.FieldError{Type: "string_type", Loc: []any{"body", "values", key}, Msg: "Input should be a valid string", Input: val})
			continue
		}
		if i, dup := index[key]; dup {
			out[i].Value = str
			continue
		}
		index[key] = len(out)
		out = append(out, settings.KV{Key: key, Value: str})
	}
	return out, errs
}

func (s integrationRoutes) readUpdate(w http.ResponseWriter, r *http.Request) ([]settings.KV, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSettingsBody))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return nil, false
	}
	vals, errs := parseIntegrationUpdate(raw)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return nil, false
	}
	return vals, true
}

func (s integrationRoutes) update(w http.ResponseWriter, r *http.Request) {
	vals, ok := s.readUpdate(w, r)
	if !ok {
		return
	}
	it, found := s.integ.Find(r.PathValue("integration_id"))
	if !found {
		httpx.Detail(w, http.StatusNotFound, "Integration not found")
		return
	}
	saved, err := s.integ.SaveGlobal(r.Context(), it, vals)
	if err != nil {
		s.fail(w, "integration update")
		return
	}
	view, err := s.integ.GlobalView(r.Context(), it)
	if err != nil {
		s.fail(w, "integration update view")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "saved": saved, "integration": view})
}

// resolveWorkspace is _resolve_active_workspace: the explicit ?workspace_id=
// or the caller's active workspace, which the caller must belong to. Like
// identity.Workspace, a workspace-scoped (SSO) session acts only in its own
// workspace (it replaces the stored active one; any other is 403), and a
// workspace with enforce_sso refuses non-SSO sessions of non-owners.
func (s integrationRoutes) resolveWorkspace(w http.ResponseWriter, r *http.Request) (string, bool) {
	p, ok := identity.PrincipalFrom(r.Context())
	if !ok {
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return "", false
	}
	ws := r.URL.Query().Get("workspace_id")
	if ws == "" {
		ws = p.Claims.Workspace
	}
	if ws == "" {
		var err error
		if ws, err = s.st.ActiveWorkspaceID(r.Context(), p.User.ID); err != nil {
			s.fail(w, "active workspace")
			return "", false
		}
	}
	if ws == "" {
		httpx.Detail(w, http.StatusBadRequest, "No active workspace")
		return "", false
	}
	if !p.Claims.AllowsWorkspace(ws) {
		httpx.Detail(w, http.StatusForbidden, "Workspace access denied")
		return "", false
	}
	member, err := s.st.IsMember(r.Context(), ws, p.User.ID)
	if err != nil {
		s.fail(w, "membership")
		return "", false
	}
	if !member {
		httpx.Detail(w, http.StatusForbidden, "Not a member of this workspace")
		return "", false
	}
	role, err := s.st.MemberRole(r.Context(), ws, p.User.ID)
	if err != nil {
		s.fail(w, "member role")
		return "", false
	}
	enforce, err := s.st.EnforceSSO(r.Context(), s.pool, ws)
	if err != nil {
		s.fail(w, "sso enforcement")
		return "", false
	}
	if enforce && role != "owner" && !slices.Contains(p.Claims.AMR, "sso") {
		httpx.Detail(w, http.StatusForbidden, ssoRequiredDetail)
		return "", false
	}
	return ws, true
}

func (s integrationRoutes) wsList(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolveWorkspace(w, r)
	if !ok {
		return
	}
	views, err := s.integ.WorkspaceViews(r.Context(), ws)
	if err != nil {
		s.fail(w, "workspace integrations list")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"workspace_id": ws, "integrations": views})
}

func (s integrationRoutes) wsUpdate(w http.ResponseWriter, r *http.Request) {
	vals, ok := s.readUpdate(w, r)
	if !ok {
		return
	}
	it, found := s.integ.Find(r.PathValue("integration_id"))
	if !found {
		httpx.Detail(w, http.StatusNotFound, "Integration not found")
		return
	}
	ws, ok := s.resolveWorkspace(w, r)
	if !ok {
		return
	}
	p, _ := identity.PrincipalFrom(r.Context())
	allowed, err := s.st.HasPermission(r.Context(), ws, p.User.ID, "integrations.manage", "admin", "editor")
	if err != nil {
		s.fail(w, "permission")
		return
	}
	if !allowed {
		httpx.Detail(w, http.StatusForbidden, "Insufficient workspace role")
		return
	}
	saved, err := s.integ.SaveWorkspace(r.Context(), ws, it, vals)
	if err != nil {
		s.fail(w, "workspace integration update")
		return
	}
	view, err := s.integ.WorkspaceView(r.Context(), ws, it)
	if err != nil {
		s.fail(w, "workspace integration update view")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "saved": saved, "workspace_id": ws, "integration": view})
}
