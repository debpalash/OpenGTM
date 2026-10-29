package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const maxBudgetBody = 1 << 20

type telemetryRoutes struct {
	d   Deps
	svc *wbcrud.Service
}

// RegisterWorkbookTelemetry registers the budget, cost, runs, activity and cell
// trace routes of routers/workbooks.py (/api/workbooks): the budget write behind
// require_editor, the reads behind current_workspace.
func RegisterWorkbookTelemetry(r *httpx.Router, d Deps) {
	if d.Pool == nil || !d.workbooksEnabled("workbook telemetry") {
		return
	}
	t := telemetryRoutes{d: d, svc: &wbcrud.Service{Pool: d.Pool, Leads: wbcrud.PGLeads{}}}
	read := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity)}
	write := append(append([]httpx.Middleware{}, read...), identity.RequireRole("tables.write", "editor", "admin"))
	const base = "/api/workbooks/{workbook_id}"
	r.Handle("PUT "+base+"/budget", http.HandlerFunc(t.setBudget), write...)
	r.Handle("GET "+base+"/cost", http.HandlerFunc(t.cost), read...)
	r.Handle("GET "+base+"/runs", http.HandlerFunc(t.runs), read...)
	r.Handle("GET "+base+"/activity", http.HandlerFunc(t.activity), read...)
	r.Handle("GET "+base+"/rows/{lead_id}/cells/{col_id}/trace", http.HandlerFunc(t.trace), read...)
}

func (t telemetryRoutes) fail(w http.ResponseWriter, what string, err error) {
	var g *wbcrud.GuardError
	if errors.As(err, &g) {
		httpx.Detail(w, g.Status, g.Detail)
		return
	}
	l := t.d.Log
	if l == nil {
		l = slog.Default()
	}
	l.Error("workbook telemetry: "+what+" failed", "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

func (t telemetryRoutes) setBudget(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBudgetBody))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return
	}
	maxUSD, errs := wbcrud.ParseBudgetBody(raw)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	ws, _ := identity.WorkspaceFrom(r.Context())
	out, err := t.svc.SetBudget(r.Context(), ws.WorkspaceID, r.PathValue("workbook_id"), maxUSD)
	if err != nil {
		t.fail(w, "set budget", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (t telemetryRoutes) cost(w http.ResponseWriter, r *http.Request) {
	ws, _ := identity.WorkspaceFrom(r.Context())
	out, err := t.svc.Cost(r.Context(), ws.WorkspaceID, r.PathValue("workbook_id"))
	if err != nil {
		t.fail(w, "cost", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func telI64(n int64) *int64 { return &n }

func (t telemetryRoutes) runs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var errs []httpx.FieldError
	// Declaration order: before_id (ge 1; Go also caps it at int4, see wbcrud.BeforeIDMax), limit (1..100, 20).
	before, e := wbcrud.QueryInt(q, "before_id", nil, telI64(1), &wbcrud.BeforeIDMax)
	if e != nil {
		errs = append(errs, *e)
	}
	limit, e := wbcrud.QueryInt(q, "limit", telI64(20), telI64(1), telI64(100))
	if e != nil {
		errs = append(errs, *e)
	}
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	var beforeID *int32
	if q.Has("before_id") {
		b := int32(before)
		beforeID = &b
	}
	ws, _ := identity.WorkspaceFrom(r.Context())
	out, err := t.svc.Runs(r.Context(), ws.WorkspaceID, r.PathValue("workbook_id"), beforeID, int(limit))
	if err != nil {
		t.fail(w, "runs", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (t telemetryRoutes) activity(w http.ResponseWriter, r *http.Request) {
	// FastAPI bounds limit only from above (le=500); a negative value is a Postgres error (500).
	// Go rejects it with 422 greater_than_equal (documented hardening).
	limit, e := wbcrud.QueryInt(r.URL.Query(), "limit", telI64(50), telI64(0), telI64(500))
	if e != nil {
		httpx.ValidationError(w, *e)
		return
	}
	ws, _ := identity.WorkspaceFrom(r.Context())
	out, err := t.svc.Activity(r.Context(), ws.WorkspaceID, r.PathValue("workbook_id"), int(limit))
	if err != nil {
		t.fail(w, "activity", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (t telemetryRoutes) trace(w http.ResponseWriter, r *http.Request) {
	lead, e := wbcrud.IntValue(r.PathValue("lead_id"), []any{"path", "lead_id"}, &wbcrud.LeadIDMin, &wbcrud.LeadIDMax)
	if e != nil {
		httpx.ValidationError(w, *e)
		return
	}
	ws, _ := identity.WorkspaceFrom(r.Context())
	out, err := t.svc.CellTrace(r.Context(), ws.WorkspaceID, r.PathValue("workbook_id"), int32(lead), r.PathValue("col_id"))
	if err != nil {
		t.fail(w, "cell trace", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
