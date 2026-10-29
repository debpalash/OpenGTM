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

const maxColumnBody = 4 << 20

// RegisterWorkbookColumns registers the column routes of /api/workbooks
// (routers/workbooks.py, "Column Management" and the column PATCH routes).
// All are behind current_workspace + require_editor and lock the workbook
// FOR UPDATE.
func RegisterWorkbookColumns(r *httpx.Router, d Deps) {
	if d.Pool == nil || !d.workbooksEnabled("workbook columns") {
		return
	}
	c := columnRoutes{d: d, svc: &wbcrud.Service{Pool: d.Pool, Leads: wbcrud.PGLeads{}}}
	mws := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity), identity.RequireRole("tables.write", "editor", "admin")}
	// "columns/order" has no {column_id} segment: ServeMux keeps it distinct from
	// the "columns/{column_id}/..." patterns (a column may itself be named "order").
	r.Handle("PATCH /api/workbooks/{workbook_id}/columns/order", http.HandlerFunc(c.order), mws...)
	r.Handle("PATCH /api/workbooks/{workbook_id}/columns/{column_id}/settings", http.HandlerFunc(c.settings), mws...)
	r.Handle("PATCH /api/workbooks/{workbook_id}/columns/{column_id}/width", http.HandlerFunc(c.width), mws...)
	r.Handle("POST /api/workbooks/{workbook_id}/columns", http.HandlerFunc(c.add), mws...)
	r.Handle("DELETE /api/workbooks/{workbook_id}/columns/{column_id}", http.HandlerFunc(c.remove), mws...)
}

type columnRoutes struct {
	d   Deps
	svc *wbcrud.Service
}

// body reads the request body, answering 413 when it is oversized.
func (c columnRoutes) body(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxColumnBody))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return nil, false
	}
	return raw, true
}

// fail answers a guard as FastAPI's HTTPException, anything else as a bare 500
// (the error text may carry SQL or column data and is never reflected).
func (c columnRoutes) fail(w http.ResponseWriter, what string, err error) {
	var g *wbcrud.GuardError
	if errors.As(err, &g) {
		httpx.Detail(w, g.Status, g.Detail)
		return
	}
	l := c.d.Log
	if l == nil {
		l = slog.Default()
	}
	l.Error("workbook columns: "+what+" failed", "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

func (c columnRoutes) settings(w http.ResponseWriter, r *http.Request) {
	raw, ok := c.body(w, r)
	if !ok {
		return
	}
	b, errs := wbcrud.ParseSettingsBody(raw)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	ws, _ := identity.WorkspaceFrom(r.Context())
	colID := r.PathValue("column_id")
	changes, err := c.svc.UpdateColumnSettings(r.Context(), ws.WorkspaceID, r.PathValue("workbook_id"), colID, b)
	if err != nil {
		c.fail(w, "settings", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		ColumnID string      `json:"column_id"`
		Changes  *wbcrud.Obj `json:"changes"`
	}{colID, changes})
}

func (c columnRoutes) order(w http.ResponseWriter, r *http.Request) {
	raw, ok := c.body(w, r)
	if !ok {
		return
	}
	b, errs := wbcrud.ParseOrderBody(raw)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	ws, _ := identity.WorkspaceFrom(r.Context())
	if err := c.svc.ReorderColumns(r.Context(), ws.WorkspaceID, r.PathValue("workbook_id"), b); err != nil {
		c.fail(w, "order", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		ColumnIDs []string `json:"column_ids"`
	}{b.ColumnIDs})
}

func (c columnRoutes) width(w http.ResponseWriter, r *http.Request) {
	raw, ok := c.body(w, r)
	if !ok {
		return
	}
	width, errs := wbcrud.ParseWidthBody(raw)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	ws, _ := identity.WorkspaceFrom(r.Context())
	colID := r.PathValue("column_id")
	if err := c.svc.SetColumnWidth(r.Context(), ws.WorkspaceID, r.PathValue("workbook_id"), colID, width); err != nil {
		c.fail(w, "width", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		ColumnID string `json:"column_id"`
		Width    int64  `json:"width"`
	}{colID, width})
}

func (c columnRoutes) add(w http.ResponseWriter, r *http.Request) {
	raw, ok := c.body(w, r)
	if !ok {
		return
	}
	col, errs := wbcrud.ParseAddBody(raw)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	ws, _ := identity.WorkspaceFrom(r.Context())
	wb, err := c.svc.AddColumn(r.Context(), ws.WorkspaceID, r.PathValue("workbook_id"), col)
	if err != nil {
		c.fail(w, "add", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, wb)
}

func (c columnRoutes) remove(w http.ResponseWriter, r *http.Request) {
	raw, ok := c.body(w, r)
	if !ok {
		return
	}
	expected, errs := wbcrud.ParseDeleteBody(raw)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	ws, _ := identity.WorkspaceFrom(r.Context())
	wb, err := c.svc.RemoveColumn(r.Context(), ws.WorkspaceID, r.PathValue("workbook_id"), r.PathValue("column_id"), expected)
	if err != nil {
		c.fail(w, "remove", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, wb)
}
