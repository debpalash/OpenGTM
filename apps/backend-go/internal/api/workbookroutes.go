package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// RegisterWorkbooks registers the workbook create/list/update/delete/from-jobs
// routes of routers/workbooks.py: GET and POST /api/workbooks/ (POST is an
// editor route), POST /api/workbooks/from-jobs, PUT and DELETE
// /api/workbooks/{workbook_id}. Nothing registers unless PG_LEAD_STORE is on.
func RegisterWorkbooks(r *httpx.Router, d Deps) {
	if d.Pool == nil || !d.workbooksEnabled("workbooks") {
		return
	}
	h := wbCRUDRoutes{d: d, svc: &wbcrud.Service{
		Pool:  d.Pool,
		Leads: wbcrud.PGLeads{},
		Hook:  automationhook.New(d.Flags.AutomationsEnabled, d.Log),
	}}
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	editor := identity.RequireRole("tables.write", "editor", "admin")

	r.Handle("GET /api/workbooks/", http.HandlerFunc(h.list), authn, workspace)
	budget := sharedBudget(d.Flags.ImportMemoryBudgetMB)
	r.Handle("POST /api/workbooks/", withBodyBudget(budget, wbcrud.MaxBodyBytes, h.create), authn, workspace, editor)
	r.Handle("POST /api/workbooks/from-jobs", withBodyBudget(budget, wbcrud.MaxBodyBytes, h.fromJobs), authn, workspace, editor)
	r.Handle("PUT /api/workbooks/{workbook_id}", withBodyBudget(budget, wbcrud.MaxBodyBytes, h.update), authn, workspace, editor)
	r.Handle("DELETE /api/workbooks/{workbook_id}", http.HandlerFunc(h.remove), authn, workspace, editor)
}

type wbCRUDRoutes struct {
	d   Deps
	svc *wbcrud.Service
}

// wsID returns the caller's workspace id (set by identity.Workspace).
func (h wbCRUDRoutes) wsID(r *http.Request) string {
	wc, _ := identity.WorkspaceFrom(r.Context())
	return wc.WorkspaceID
}

// fail maps a service error to its FastAPI answer; unknown errors become 500
// without leaking the error text.
func (h wbCRUDRoutes) fail(w http.ResponseWriter, what string, err error) {
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
		l.Error("workbooks: "+what+" failed", "err_type", fmt.Sprintf("%T", err))
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
	}
}

// body reads and returns the request body, answering 413 when oversized.
func (h wbCRUDRoutes) body(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, ok := wbcrud.ReadBody(r.Body)
	if !ok {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
	}
	return raw, ok
}

func (h wbCRUDRoutes) list(w http.ResponseWriter, r *http.Request) {
	ws := h.wsID(r)
	var out []wbcrud.Workbook
	err := h.svc.WithTx(r.Context(), ws, func(tx pgx.Tx) (err error) {
		out, err = h.svc.List(r.Context(), tx, ws)
		return err
	})
	if err != nil {
		h.fail(w, "list", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		Workbooks []wbcrud.Workbook `json:"workbooks"`
		Total     int               `json:"total"`
	}{out, len(out)})
}

func (h wbCRUDRoutes) create(w http.ResponseWriter, r *http.Request) {
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	body, errs := wbcrud.ParseCreateBody(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	wb, err := h.svc.Create(r.Context(), h.wsID(r), body)
	if err != nil {
		h.fail(w, "create", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, wb)
}

func (h wbCRUDRoutes) update(w http.ResponseWriter, r *http.Request) {
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	body, errs := wbcrud.ParseUpdateBody(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	wb, err := h.svc.Update(r.Context(), h.wsID(r), r.PathValue("workbook_id"), body)
	if err != nil {
		h.fail(w, "update", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, wb)
}

func (h wbCRUDRoutes) remove(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Delete(r.Context(), h.wsID(r), r.PathValue("workbook_id")); err != nil {
		h.fail(w, "delete", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h wbCRUDRoutes) fromJobs(w http.ResponseWriter, r *http.Request) {
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	body, errs := wbcrud.ParseFromJobsBody(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	wb, err := h.svc.FromJobs(r.Context(), h.wsID(r), body)
	if err != nil {
		h.fail(w, "from-jobs", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, wb)
}
