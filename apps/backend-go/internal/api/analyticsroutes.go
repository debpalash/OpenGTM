package api

import (
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/analytics"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// RegisterAnalytics registers the five GET /api/analytics routes (routers/analytics.py), Postgres lead
// store branch only; each sits behind current_workspace. Nothing registers when PG_LEAD_STORE is off.
func RegisterAnalytics(r *httpx.Router, d Deps) {
	if d.Pool == nil || !d.workbooksEnabled("analytics") {
		return
	}
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	h := leadReadRoutes{d: d}
	serve := func(what string, fn func(r *http.Request, tx pgx.Tx) (any, error)) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			var out any
			wc, _ := identity.WorkspaceFrom(req.Context())
			err := tenancy.InWorkspace(req.Context(), d.Pool, wc.WorkspaceID, func(tx pgx.Tx) (err error) {
				out, err = fn(req, tx)
				return err
			})
			if err != nil {
				h.fail(w, "analytics "+what, err)
				return
			}
			httpx.WriteJSON(w, http.StatusOK, out)
		})
	}
	r.Handle("GET /api/analytics/overview", serve("overview", func(q *http.Request, tx pgx.Tx) (any, error) { return analytics.Overview(q.Context(), tx) }), authn, workspace)
	r.Handle("GET /api/analytics/pipeline", serve("pipeline", func(q *http.Request, tx pgx.Tx) (any, error) { return analytics.Pipeline(q.Context(), tx) }), authn, workspace)
	r.Handle("GET /api/analytics/enrichment", serve("enrichment", func(q *http.Request, tx pgx.Tx) (any, error) { return analytics.Enrichment(q.Context(), tx) }), authn, workspace)
	r.Handle("GET /api/analytics/collection", serve("collection", func(*http.Request, pgx.Tx) (any, error) { return analytics.Collection(), nil }), authn, workspace)
	r.Handle("GET /api/analytics/llm", serve("llm", func(*http.Request, pgx.Tx) (any, error) { return analytics.LLM(), nil }), authn, workspace)
}
