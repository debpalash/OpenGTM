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

const maxViewBody = 4 << 20

// RegisterWorkbookViews registers the saved-view routes (routers/workbooks.py,
// views_router under /api/v2/workbooks): list behind current_workspace, the
// writes behind require_editor.
func RegisterWorkbookViews(r *httpx.Router, d Deps) {
	if d.Pool == nil || !d.workbooksEnabled("workbook views") {
		return
	}
	v := viewRoutes{d: d, svc: &wbcrud.Service{Pool: d.Pool, Leads: wbcrud.PGLeads{}}}
	read := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity)}
	write := append(append([]httpx.Middleware{}, read...), identity.RequireRole("tables.write", "editor", "admin"))
	const base = "/api/v2/workbooks/{workbook_id}/views"
	r.Handle("GET "+base, http.HandlerFunc(v.list), read...)
	r.Handle("POST "+base, http.HandlerFunc(v.create), write...)
	r.Handle("PUT "+base+"/{view_id}", http.HandlerFunc(v.update), write...)
	r.Handle("DELETE "+base+"/{view_id}", http.HandlerFunc(v.remove), write...)
}

type viewRoutes struct {
	d   Deps
	svc *wbcrud.Service
}

func (v viewRoutes) fail(w http.ResponseWriter, what string, err error) {
	var g *wbcrud.GuardError
	if errors.As(err, &g) {
		httpx.Detail(w, g.Status, g.Detail)
		return
	}
	l := v.d.Log
	if l == nil {
		l = slog.Default()
	}
	l.Error("workbook views: "+what+" failed", "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

func (v viewRoutes) body(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxViewBody))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return nil, false
	}
	return raw, true
}

func (v viewRoutes) list(w http.ResponseWriter, r *http.Request) {
	ws, _ := identity.WorkspaceFrom(r.Context())
	views, err := v.svc.ListViews(r.Context(), ws.WorkspaceID, r.PathValue("workbook_id"))
	if err != nil {
		v.fail(w, "list", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		Views []wbcrud.View `json:"views"`
		Total int           `json:"total"`
	}{views, len(views)})
}

func (v viewRoutes) create(w http.ResponseWriter, r *http.Request) {
	raw, ok := v.body(w, r)
	if !ok {
		return
	}
	b, errs := wbcrud.ParseViewCreateBody(raw)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	ws, _ := identity.WorkspaceFrom(r.Context())
	out, err := v.svc.CreateView(r.Context(), ws.WorkspaceID, r.PathValue("workbook_id"), b)
	if err != nil {
		v.fail(w, "create", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

func (v viewRoutes) update(w http.ResponseWriter, r *http.Request) {
	raw, ok := v.body(w, r)
	if !ok {
		return
	}
	b, errs := wbcrud.ParseViewUpdateBody(raw)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	ws, _ := identity.WorkspaceFrom(r.Context())
	out, err := v.svc.UpdateView(r.Context(), ws.WorkspaceID, r.PathValue("workbook_id"), r.PathValue("view_id"), b)
	if err != nil {
		v.fail(w, "update", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (v viewRoutes) remove(w http.ResponseWriter, r *http.Request) {
	ws, _ := identity.WorkspaceFrom(r.Context())
	if err := v.svc.DeleteView(r.Context(), ws.WorkspaceID, r.PathValue("workbook_id"), r.PathValue("view_id")); err != nil {
		v.fail(w, "delete", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
	}{"deleted"})
}
