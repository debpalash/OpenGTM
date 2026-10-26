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
