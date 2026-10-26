package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbregistry"
	"github.com/jackc/pgx/v5"
)

// RegisterWorkbookMeta registers the static workbook meta routes
// (/api/workbooks/meta/*, routers/workbooks.py) and the two foundation routes
// of routers/meta.py (/api/me/context, /api/flags), all behind current_workspace.
// GET /meta/provider-stats and /meta/provider-accuracy stay on FastAPI. The meta
// literals are more specific than GET /api/workbooks/{workbook_id}, so they win.
func RegisterWorkbookMeta(r *httpx.Router, d Deps) {
	if d.Pool == nil || d.Identity == nil {
		return
	}
	read := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity)}
	m := metaRoutes{d: d}
	// me/context and flags do not touch workbooks; register them regardless of
	// the lead-store gate so the SPA keeps its proactive gating.
	r.Handle("GET /api/me/context", http.HandlerFunc(m.meContext), read...)
	r.Handle("GET /api/flags", http.HandlerFunc(m.flags), read...)
	if !d.workbooksEnabled("workbook meta") {
		return
	}
	reg, err := wbregistry.Load()
	if err != nil {
		l := d.Log
		if l == nil {
			l = slog.Default()
		}
		l.Error("workbook meta: registry unavailable, routes not registered", "err_type", errTypeName(err))
		return
	}
	m.reg = reg
	const base = "GET /api/workbooks/meta/"
	r.Handle(base+"column-types", http.HandlerFunc(m.columnTypes), read...)
	r.Handle(base+"lead-fields", http.HandlerFunc(m.leadFields), read...)
	r.Handle(base+"providers", http.HandlerFunc(m.providers), read...)
	r.Handle(base+"ai-column-presets", http.HandlerFunc(m.aiPresets), read...)
	r.Handle(base+"filter-options", http.HandlerFunc(m.filterOptions), read...)
}

type metaRoutes struct {
	d   Deps
	reg *wbregistry.Registry
}

func (m metaRoutes) columnTypes(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, m.reg.ColumnTypes())
}

func (m metaRoutes) leadFields(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, struct {
		Fields []string `json:"fields"`
	}{m.reg.LeadFields()})
}

func (m metaRoutes) providers(w http.ResponseWriter, r *http.Request) {
	ps := m.reg.Providers()
	if ps == nil {
		ps = []wbregistry.Provider{}
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		Providers []wbregistry.Provider `json:"providers"`
	}{ps})
}

func (m metaRoutes) aiPresets(w http.ResponseWriter, r *http.Request) {
	presets, cats := m.reg.AIPresets(r.URL.Query().Get("category"))
	httpx.WriteJSON(w, http.StatusOK, struct {
		Presets    json.RawMessage `json:"presets"`
		Categories json.RawMessage `json:"categories"`
	}{presets, cats})
}

func (m metaRoutes) filterOptions(w http.ResponseWriter, r *http.Request) {
	ws, _ := identity.WorkspaceFrom(r.Context())
	var out map[string]any
	err := ws.Tx(r.Context(), func(tx pgx.Tx) (err error) {
		out, err = leadsread.FilterOptions(r.Context(), tx)
		return err
	})
	if err != nil {
		l := m.d.Log
		if l == nil {
			l = slog.Default()
		}
		l.Error("workbook meta: filter options failed", "err_type", errTypeName(err))
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	// Python key order (the helper returns a map, which would sort alphabetically).
	httpx.WriteJSON(w, http.StatusOK, struct {
		Cities          any `json:"cities"`
		Tiers           any `json:"tiers"`
		Sources         any `json:"sources"`
		Statuses        any `json:"statuses"`
		Specializations any `json:"specializations"`
		TotalLeads      any `json:"total_leads"`
	}{out["cities"], out["tiers"], out["sources"], out["statuses"], out["specializations"], out["total_leads"]})
}

func (m metaRoutes) meContext(w http.ResponseWriter, r *http.Request) {
	ws, _ := identity.WorkspaceFrom(r.Context())
	role := ws.Role
	if role == "" {
		role = "member" // least privilege when the member row is missing
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		UserID      string `json:"user_id"`
		WorkspaceID string `json:"workspace_id"`
		Role        string `json:"role"`
		IsOwner     bool   `json:"is_owner"`
	}{strconv.Itoa(int(ws.User.ID)), ws.WorkspaceID, role, role == "owner"})
}

func (m metaRoutes) flags(w http.ResponseWriter, r *http.Request) {
	f := m.d.Flags
	httpx.WriteJSON(w, http.StatusOK, struct {
		AutomationsEnabled  bool `json:"automations_enabled"`
		IntentPollerEnabled bool `json:"intent_poller_enabled"`
		PGLeadStore         bool `json:"pg_lead_store"`
		AllowLegacyOutreach bool `json:"allow_legacy_outreach"`
	}{f.AutomationsEnabled, f.IntentPollerEnabled, f.PGLeadStore, f.AutomationsAllowLegacyOutreach})
}
