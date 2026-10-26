package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/vendorcatalog"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// registerWorkbookEstimate registers GET .../run/estimate (current_workspace),
// POST .../stop (require_editor) and GET /api/workbooks/meta/provider-stats
// (no per-route dependency: it reads the global provider_stats table, and the
// global auth middleware of the process is the only gate, as in Python).
// provider-stats is a more specific literal than GET /api/workbooks/{id}.
func registerWorkbookEstimate(r *httpx.Router, d Deps) {
	if d.Pool == nil || d.Identity == nil {
		return
	}
	h := estimateRoutes{d: d, svc: &wbcrud.Service{Pool: d.Pool, Leads: wbcrud.PGLeads{}}}
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	editor := identity.RequireRole("tables.write", "editor", "admin")
	r.Handle("GET /api/workbooks/meta/provider-stats", http.HandlerFunc(h.providerStats))
	if !d.workbooksEnabled("workbook estimate") {
		return
	}
	r.Handle("GET /api/workbooks/{workbook_id}/run/estimate", http.HandlerFunc(h.estimate), authn, workspace)
	r.Handle("POST /api/workbooks/{workbook_id}/stop", http.HandlerFunc(h.stop), authn, workspace, editor)
}

type estimateRoutes struct {
	d   Deps
	svc *wbcrud.Service
}

func (h estimateRoutes) fail(w http.ResponseWriter, what string, err error) {
	var g *wbcrud.GuardError
	switch {
	case errors.Is(err, wbcrud.ErrNotFound):
		httpx.Detail(w, http.StatusNotFound, wbcrud.NotFoundDetail)
	case errors.As(err, &g):
		httpx.Detail(w, g.Status, g.Detail)
	default:
		l := h.d.Log
		if l == nil {
			l = slog.Default()
		}
		l.Error("workbook estimate: "+what+" failed", "err_type", fmt.Sprintf("%T", err))
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
	}
}

// enrichmentColTypes is enrichment.ENRICHMENT_COL_TYPES.
var enrichmentColTypes = map[string]bool{"enrichment": true, "waterfall": true, "ai_formula": true, "research": true,
	"agent": true, "http": true, "formula": true, "output": true}

func truthyString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok && s != ""
}

// estimateChains is the providers_by_col loop of estimate_run: an explicit
// (even empty) waterfall disables provider selection, only an absent or null
// one falls back to the single provider or the target's default waterfall.
// Duplicate column ids keep the first position and the last chain, like a dict.
func estimateChains(cols []wbcrud.Column) (map[string][]string, []string, error) {
	byCol := map[string][]string{}
	var order []string
	for _, c := range cols {
		typ, _ := c.Get("type").(string)
		if !enrichmentColTypes[typ] {
			continue
		}
		id, _ := truthyString(c.Get("id"))
		target, ok := truthyString(c.Get("target_field"))
		if !ok {
			if target, ok = truthyString(c.Get("lead_field")); !ok {
				target = id
			}
		}
		var chain []string
		if wf, present := c.Lookup("waterfall"); present && wf != nil {
			list, ok := wf.([]any)
			if !ok {
				return nil, nil, errors.New("estimate: waterfall is not a list")
			}
			chain = make([]string, 0, len(list))
			for _, e := range list {
				s, ok := e.(string)
				if !ok {
					return nil, nil, errors.New("estimate: waterfall entry is not a string")
				}
				chain = append(chain, s)
			}
		} else if p, ok := truthyString(c.Get("provider")); ok {
			chain = []string{p}
		} else {
			chain = workbook.DefaultWaterfall(target)
		}
		key := id
		if key == "" {
			key = target
		}
		if _, seen := byCol[key]; !seen {
			order = append(order, key)
		}
		byCol[key] = chain
	}
	return byCol, order, nil
}

func (h estimateRoutes) estimate(w http.ResponseWriter, r *http.Request) {
	v := &queryValidator{q: r.URL.Query()}
	viewID, _ := v.text("view_id", 0)
	search, _ := v.text("search", 500)
	if v.fail(w) {
		return
	}
	wc, _ := identity.WorkspaceFrom(r.Context())
	id := r.PathValue("workbook_id")
	var out *wbcrud.Obj
	err := h.svc.WithTx(r.Context(), wc.WorkspaceID, func(tx pgx.Tx) error {
		wb, err := h.svc.Owned(r.Context(), tx, wc.WorkspaceID, id, false)
		if err != nil {
			return err
		}
		rows, err := h.svc.CountRows(r.Context(), tx, wb, viewID, search)
		if err != nil {
			return err
		}
		byCol, order, err := estimateChains(wb.Columns)
		if err != nil {
			return err
		}
		out = vendorcatalog.EstimateRunCost(rows, byCol, order)
		out.Set("workbook_id", id)
		return nil
	})
	if err != nil {
		h.fail(w, "estimate", err)
		return
	}
	writeOrdered(w, out)
}

// roundDecimals is Python's round(x, nd) for a float: exact decimal rounding.
func roundDecimals(x float64, nd int) float64 {
	f, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', nd, 64), 64)
	if err != nil {
		return x
	}
	return f
}

func (h estimateRoutes) providerStats(w http.ResponseWriter, r *http.Request) {
	rows, err := gen.New(h.d.Pool).ListProviderStats(r.Context())
	if err != nil {
		h.fail(w, "provider stats", err)
		return
	}
	stats := make([]any, 0, len(rows))
	for _, s := range rows {
		attempts, hits := int(s.Attempts.Int32), int(s.Hits.Int32)
		hitRate, avgConf, avgLat := 0.0, 0.0, 0.0
		if attempts != 0 {
			hitRate = float64(hits) / float64(attempts)
			avgLat = s.TotalLatencyMs.Float64 / float64(attempts)
		}
		if hits != 0 {
			avgConf = s.TotalConfidence.Float64 / float64(hits)
		}
		o := wbcrud.NewObj()
		o.Set("provider", s.Provider)
		o.Set("field", s.Field)
		if s.Attempts.Valid {
			o.Set("attempts", attempts)
		} else {
			o.Set("attempts", nil)
		}
		if s.Hits.Valid {
			o.Set("hits", hits)
		} else {
			o.Set("hits", nil)
		}
		o.Set("hit_rate", roundDecimals(hitRate, 3))
		o.Set("avg_confidence", roundDecimals(avgConf, 3))
		o.Set("avg_latency_ms", roundDecimals(avgLat, 1))
		o.Set("total_cost_usd", roundDecimals(s.TotalCostUsd.Float64, 4))
		if s.AccuracyScore.Valid {
			o.Set("accuracy_score", roundDecimals(s.AccuracyScore.Float64, 3))
		} else {
			o.Set("accuracy_score", nil)
		}
		o.Set("accuracy_samples", int(s.AccuracySamples.Int32))
		if s.CooldownUntil.Valid {
			o.Set("cooldown_until", naiveISO(s.CooldownUntil.Time))
		} else {
			o.Set("cooldown_until", nil)
		}
		stats = append(stats, o)
	}
	out := wbcrud.NewObj()
	out.Set("stats", stats)
	writeOrdered(w, out)
}
