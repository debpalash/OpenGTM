package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// maxImportBody caps an import upload (413 beyond it). FastAPI has no limit;
// 50,000 rows must never be able to exhaust memory, so the cap is part of the
// contract (documented in testdata/contract/README.md).
const maxImportBody = 64 << 20

// importBudgetWait is how long a request waits for upload budget before 503.
var importBudgetWait = 5 * time.Second

type importRoutes struct {
	d      Deps
	svc    *wbcrud.Service
	budget *byteBudget
	wait   time.Duration
}

// RegisterWorkbookImport registers POST /{id}/import/preview and /{id}/import
// of routers/workbooks.py. Both are behind current_workspace + require_editor.
func RegisterWorkbookImport(r *httpx.Router, d Deps) {
	if d.Pool == nil || !d.workbooksEnabled("workbook import") {
		return
	}
	ir := importRoutes{d: d, budget: sharedBudget(d.Flags.ImportMemoryBudgetMB), wait: importBudgetWait}
	ir.svc = &wbcrud.Service{Pool: d.Pool, Leads: wbcrud.PGLeads{}, Hook: automationhook.New(d.Flags.AutomationsEnabled, d.Log), Now: time.Now}
	mws := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity),
		identity.RequireRole("tables.write", "editor", "admin")}
	const base = "/api/workbooks/{workbook_id}/import"
	r.Handle("POST "+base+"/preview", http.HandlerFunc(ir.withBudget(ir.preview)), mws...)
	r.Handle("POST "+base, http.HandlerFunc(ir.withBudget(ir.run)), mws...)
}

func (ir importRoutes) log() *slog.Logger {
	if ir.d.Log != nil {
		return ir.d.Log
	}
	return slog.Default()
}

// withBudget reserves upload bytes (Content-Length, or the body cap when the
// length is unknown) before the body is read, after authentication and the role
// gate. It answers 503 + Retry-After when the budget stays full, and releases
// when the handler returns.
func (ir importRoutes) withBudget(h func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxImportBody {
			httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
			return
		}
		weight := r.ContentLength
		if weight < 0 {
			weight = maxImportBody
		}
		ctx, cancel := context.WithTimeout(r.Context(), ir.wait)
		release, ok := ir.budget.acquire(ctx, weight)
		cancel()
		if !ok {
			if r.Context().Err() != nil {
				return // the client went away while waiting
			}
			httpx.Detail(w, http.StatusServiceUnavailable, "Too many imports in progress", httpx.Header{Key: "Retry-After", Value: "5"})
			return
		}
		defer release()
		h(w, r)
	}
}

// request streams and validates the body, answering 413/422 itself.
func (ir importRoutes) request(w http.ResponseWriter, r *http.Request) (wbcrud.ImportRequest, bool) {
	if r.ContentLength > maxImportBody {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return wbcrud.ImportRequest{}, false
	}
	req, errs, err := wbcrud.ReadImportRequest(r.Body, maxImportBody)
	switch {
	case errors.Is(err, wbcrud.ErrImportBodyTooLarge):
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return req, false
	case err != nil: // the client went away or the connection broke mid-upload
		httpx.Detail(w, http.StatusBadRequest, "Invalid request body")
		return req, false
	case errs != nil:
		httpx.ValidationError(w, errs...)
		return req, false
	}
	return req, true
}

// answer maps a service outcome: ErrNotFound -> 404, guard -> its status,
// anything else -> 500. It reports whether the request succeeded.
func (ir importRoutes) answer(w http.ResponseWriter, what string, guard *wbcrud.GuardError, err error) bool {
	switch {
	case err == nil && guard == nil:
		return true
	case errors.Is(err, wbcrud.ErrNotFound):
		httpx.Detail(w, http.StatusNotFound, wbcrud.NotFoundDetail)
	case guard != nil:
		httpx.Detail(w, guard.Status, guard.Detail)
	default:
		ir.log().Error("workbook import: "+what+" failed", "err_type", errTypeName(err))
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
	}
	return false
}

func (ir importRoutes) preview(w http.ResponseWriter, r *http.Request) {
	req, ok := ir.request(w, r)
	if !ok {
		return
	}
	wsc, _ := identity.WorkspaceFrom(r.Context())
	ws := wsc.WorkspaceID
	var out *wbcrud.Obj
	err := ir.svc.WithTx(r.Context(), ws, func(tx pgx.Tx) error {
		wb, err := ir.svc.Owned(r.Context(), tx, ws, r.PathValue("workbook_id"), false)
		if err != nil {
			return err
		}
		out = ir.svc.PreviewImport(wb, &req)
		return nil
	})
	if ir.answer(w, "preview", nil, err) {
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}

var errImportGuard = errors.New("guard")

func (ir importRoutes) run(w http.ResponseWriter, r *http.Request) {
	req, ok := ir.request(w, r)
	if !ok {
		return
	}
	wsc, _ := identity.WorkspaceFrom(r.Context())
	ws := wsc.WorkspaceID
	var (
		out   *wbcrud.Obj
		guard *wbcrud.GuardError
	)
	err := ir.svc.WithTx(r.Context(), ws, func(tx pgx.Tx) error {
		wb, err := ir.svc.Owned(r.Context(), tx, ws, r.PathValue("workbook_id"), true)
		if err != nil {
			return err
		}
		var g *wbcrud.GuardError
		out, g, err = wbcrud.Importer{Service: ir.svc}.Import(r.Context(), tx, ws, wb, &req)
		if err != nil {
			return err
		}
		if g != nil {
			guard = g
			return errImportGuard // roll back: nothing a refused import did may stay
		}
		return nil
	})
	if errors.Is(err, errImportGuard) {
		err = nil
	}
	if ir.answer(w, "import", guard, err) {
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}
