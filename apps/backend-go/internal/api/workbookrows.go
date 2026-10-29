package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/runstart"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// maxRowsBody bounds every row-route request body (add rows has no row cap in
// FastAPI; this keeps memory bounded: 413 beyond it).
const maxRowsBody = 32 << 20

type rowRoutes struct {
	d   Deps
	svc *wbcrud.Service
	rec *wbcrud.Recomputer
}

// RegisterWorkbookRows registers the row routes of routers/workbooks.py
// (add/delete rows, delete-query, row and bulk patch, lead field update). All
// are behind current_workspace + require_editor.
func RegisterWorkbookRows(r *httpx.Router, d Deps) {
	if d.Pool == nil || !d.workbooksEnabled("workbook rows") {
		return
	}
	rr := rowRoutes{d: d, rec: &wbcrud.Recomputer{BillingEnabled: d.Config.BillingEnabled}}
	rr.svc = &wbcrud.Service{Pool: d.Pool, Leads: wbcrud.PGLeads{}, Hook: d.automationHook(), Now: time.Now}
	if d.Registry != nil && d.Envelope != nil {
		app := &settings.App{Store: secrets.NewPgStore(d.Pool), Env: d.Envelope, Secret: d.Registry.SecretSet(), Getenv: d.getenv()}
		perf := &settings.Perf{App: app, Reg: d.Registry}
		rr.rec.Perf = func(ctx context.Context) (wbcrud.PerfValues, error) {
			p, err := perf.Get(ctx)
			return wbcrud.PerfValues{RowConcurrency: p.RowConcurrency, ProviderWorkers: p.ProviderWorkers,
				ProviderTimeout: p.ProviderTimeout, MaxProviders: p.MaxProviders, RetryPasses: p.RetryPasses}, err
		}
	}
	if d.Config.BillingEnabled && d.Flags.GoRunRoutesEnabled {
		// With platform billing the recompute is the run route's plan, debit and enqueue (T31).
		runs := newRunRoutes(d)
		rr.rec.Run = runstart.Recompute(runstart.RecomputeDeps{BillingEnabled: true, Config: runs.config,
			Enqueue: func(ctx context.Context, tx pgx.Tx, ws, executor string, payload json.RawMessage) (int64, error) {
				return runEnqueue(ctx, tx, ws, executor, payload)
			}})
	}
	mws := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity),
		identity.RequireRole("tables.write", "editor", "admin")}
	const base = "/api/workbooks/{workbook_id}"
	r.Handle("POST "+base+"/rows", withBodyBudget(sharedBudget(d.Flags.ImportMemoryBudgetMB), maxRowsBody, rr.add), mws...)
	r.Handle("DELETE "+base+"/rows", http.HandlerFunc(rr.deleteRows), mws...)
	r.Handle("POST "+base+"/rows/delete-query", http.HandlerFunc(rr.deleteQuery), mws...)
	r.Handle("PATCH "+base+"/rows/{row_id}", http.HandlerFunc(rr.patchRow), mws...)
	r.Handle("PATCH "+base+"/rows", http.HandlerFunc(rr.bulkPatch), mws...)
	r.Handle("PUT "+base+"/leads/{lead_id}", http.HandlerFunc(rr.updateLead), mws...)
}

func (rr rowRoutes) log() *slog.Logger {
	if rr.d.Log != nil {
		return rr.d.Log
	}
	return slog.Default()
}

func (rr rowRoutes) fail(w http.ResponseWriter, what string, err error) {
	rr.log().Error("workbook rows: "+what+" failed", "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

func readRowsBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRowsBody))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return nil, false
	}
	return raw, true
}

// body reads and decodes the request body, returning the tree or its envelope errors.
func readRowsDoc(w http.ResponseWriter, r *http.Request) (any, []httpx.FieldError, bool) {
	raw, ok := readRowsBody(w, r)
	if !ok {
		return nil, nil, false
	}
	doc, errs := wbcrud.DecodeBody(raw)
	return doc, errs, true
}

// run opens the workspace tx, loads the workbook (404 "Workbook not found") and
// runs fn. A *GuardError or *wbcrud.CountDrift from fn is answered here (the tx
// rolls back); any other error is a 500. It reports whether the request was
// answered successfully by fn's caller.
func (rr rowRoutes) run(w http.ResponseWriter, r *http.Request, forUpdate bool, fn func(ctx context.Context, tx pgx.Tx, ws string, wb wbcrud.Workbook) (*wbcrud.GuardError, error)) bool {
	wsc, _ := identity.WorkspaceFrom(r.Context())
	ws := wsc.WorkspaceID
	var guard *wbcrud.GuardError
	err := rr.svc.WithTx(r.Context(), ws, func(tx pgx.Tx) error {
		wb, err := rr.svc.Owned(r.Context(), tx, ws, r.PathValue("workbook_id"), forUpdate)
		if err != nil {
			return err
		}
		g, err := fn(r.Context(), tx, ws, wb)
		if err != nil {
			return err
		}
		if g != nil {
			guard = g
			return errGuard
		}
		return nil
	})
	var drift *wbcrud.CountDrift
	switch {
	case err == nil:
		return true
	case errors.Is(err, wbcrud.ErrNotFound):
		httpx.Detail(w, http.StatusNotFound, wbcrud.NotFoundDetail)
	case errors.Is(err, errGuard):
		httpx.Detail(w, guard.Status, guard.Detail)
	case errors.As(err, &drift):
		httpx.WriteJSON(w, http.StatusConflict, map[string]any{"detail": driftDetail{
			Message: wbcrud.DriftMessage, ExpectedCount: drift.Expected, ActualCount: drift.Actual}})
	default:
		rr.fail(w, "request", err)
	}
	return false
}

var errGuard = errors.New("guard")

type driftDetail struct {
	Message       string `json:"message"`
	ExpectedCount int64  `json:"expected_count"`
	ActualCount   int64  `json:"actual_count"`
}

func (rr rowRoutes) add(w http.ResponseWriter, r *http.Request) {
	doc, errs, ok := readRowsDoc(w, r)
	if !ok {
		return
	}
	var req wbcrud.AddRowsRequest
	if errs == nil {
		req, errs = wbcrud.ParseAddRows(doc)
	}
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	var res wbcrud.AddRowsResult
	if rr.run(w, r, true, func(ctx context.Context, tx pgx.Tx, ws string, wb wbcrud.Workbook) (*wbcrud.GuardError, error) {
		var err error
		res, err = rr.svc.AddRows(ctx, tx, ws, wb, req)
		return nil, err
	}) {
		httpx.WriteJSON(w, http.StatusOK, res)
	}
}

func (rr rowRoutes) deleteRows(w http.ResponseWriter, r *http.Request) {
	doc, errs, ok := readRowsDoc(w, r)
	if !ok {
		return
	}
	var req wbcrud.DeleteRowsRequest
	if errs == nil {
		req, errs = wbcrud.ParseDeleteRows(doc)
	}
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	var deleted int64
	if rr.run(w, r, true, func(ctx context.Context, tx pgx.Tx, ws string, wb wbcrud.Workbook) (*wbcrud.GuardError, error) {
		var err error
		deleted, err = rr.svc.DeleteRows(ctx, tx, ws, wb, req.IDs)
		return nil, err
	}) {
		audit.SetMetadata(r.Context(), map[string]any{"action": "workbook.rows.delete", "workbook_id": r.PathValue("workbook_id"),
			"requested_count": req.Requested, "deleted_count": deleted})
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
	}
}

func (rr rowRoutes) deleteQuery(w http.ResponseWriter, r *http.Request) {
	doc, errs, ok := readRowsDoc(w, r)
	if !ok {
		return
	}
	var req wbcrud.DeleteMatchingRequest
	if errs == nil {
		req, errs = wbcrud.ParseDeleteMatching(doc)
	}
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	var res wbcrud.DeleteMatchingResult
	if rr.run(w, r, true, func(ctx context.Context, tx pgx.Tx, ws string, wb wbcrud.Workbook) (*wbcrud.GuardError, error) {
		var (
			g   *wbcrud.GuardError
			err error
		)
		res, g, err = rr.svc.DeleteMatching(ctx, tx, ws, wb, req)
		return g, err
	}) {
		var view any
		if req.ViewID != nil {
			view = *req.ViewID
		}
		applied := false
		if req.Search != nil {
			applied = len(trimSpace(*req.Search)) > 0
		}
		audit.SetMetadata(r.Context(), map[string]any{"action": "workbook.rows.delete_matching", "workbook_id": r.PathValue("workbook_id"),
			"view_id": view, "search_applied": applied, "matched_count": res.Matched, "deleted_count": res.Deleted})
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"deleted": res.Deleted, "matched": res.Matched})
	}
}

// startRecompute runs the post-commit recompute (a separate transaction, as in
// Python where the row data is already committed). A guard Python would raise
// as an HTTPException answers the request.
func (rr rowRoutes) startRecompute(w http.ResponseWriter, r *http.Request, columns []any, rowIDs []int64, rowColumns *wbcrud.Obj) (*wbcrud.Obj, bool) {
	var out *wbcrud.Obj
	ok := rr.run(w, r, false, func(ctx context.Context, tx pgx.Tx, ws string, wb wbcrud.Workbook) (*wbcrud.GuardError, error) {
		var (
			g   *wbcrud.GuardError
			err error
		)
		out, g, err = rr.rec.Start(ctx, tx, ws, wb, columns, rowIDs, rowColumns)
		return g, err
	})
	return out, ok
}

type rowPatchResponse struct {
	Status          string   `json:"status"`
	RowID           int64    `json:"row_id"`
	Fields          []string `json:"fields"`
	ReactiveColumns []any    `json:"reactive_columns"`
	Recompute       any      `json:"recompute"`
}

func (rr rowRoutes) patchRow(w http.ResponseWriter, r *http.Request) {
	var errs []httpx.FieldError
	rowID, fe := wbcrud.PathInt(r.PathValue("row_id"), "row_id")
	if fe != nil {
		errs = append(errs, *fe)
	}
	recompute, fe := wbcrud.RecomputeParam(r.URL.Query())
	if fe != nil {
		errs = append(errs, *fe)
	}
	doc, benv, ok := readRowsDoc(w, r)
	if !ok {
		return
	}
	var body *wbcrud.Obj
	if benv != nil {
		errs = append(errs, benv...)
	} else {
		var be []httpx.FieldError
		if body, be = wbcrud.ParseDictBody(doc); be != nil {
			errs = append(errs, be...)
		}
	}
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	var res wbcrud.RowPatch
	if !rr.run(w, r, false, func(ctx context.Context, tx pgx.Tx, ws string, wb wbcrud.Workbook) (*wbcrud.GuardError, error) {
		var (
			g   *wbcrud.GuardError
			err error
		)
		res, g, err = rr.svc.PatchRow(ctx, tx, ws, wb, rowID, body, recompute)
		return g, err
	}) {
		return
	}
	var recomputeOut any
	if recompute && len(res.Reactive) > 0 {
		out, ok := rr.startRecompute(w, r, res.Reactive, []int64{rowID}, nil)
		if !ok {
			return
		}
		recomputeOut = out
	}
	names := append([]string(nil), res.Fields...)
	sort.Strings(names)
	audit.SetMetadata(r.Context(), map[string]any{"action": "workbook.row.update", "workbook_id": r.PathValue("workbook_id"),
		"row_id": rowID, "field_names": names, "reactive_columns": res.Reactive})
	httpx.WriteJSON(w, http.StatusOK, rowPatchResponse{Status: "updated", RowID: rowID, Fields: res.Fields,
		ReactiveColumns: res.Reactive, Recompute: recomputeOut})
}

type bulkPatchResponse struct {
	Status          string `json:"status"`
	UpdatedRows     int    `json:"updated_rows"`
	ReactiveColumns []any  `json:"reactive_columns"`
	Recompute       any    `json:"recompute"`
}

func (rr rowRoutes) bulkPatch(w http.ResponseWriter, r *http.Request) {
	var errs []httpx.FieldError
	recompute, fe := wbcrud.RecomputeParam(r.URL.Query())
	if fe != nil {
		errs = append(errs, *fe)
	}
	doc, benv, ok := readRowsDoc(w, r)
	if !ok {
		return
	}
	var req wbcrud.BulkUpdateRequest
	if benv != nil {
		errs = append(errs, benv...)
	} else {
		var be []httpx.FieldError
		if req, be = wbcrud.ParseBulkUpdate(doc); be != nil {
			errs = append(errs, be...)
		}
	}
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	var res wbcrud.BulkPatch
	if !rr.run(w, r, false, func(ctx context.Context, tx pgx.Tx, ws string, wb wbcrud.Workbook) (*wbcrud.GuardError, error) {
		var (
			g   *wbcrud.GuardError
			err error
		)
		res, g, err = rr.svc.BulkPatchRows(ctx, tx, ws, wb, req.Updates, recompute)
		return g, err
	}) {
		return
	}
	var recomputeOut any
	if recompute && len(res.Reactive) > 0 {
		out, ok := rr.startRecompute(w, r, res.Reactive, res.RowIDs, res.RowColumns)
		if !ok {
			return
		}
		recomputeOut = out
	}
	changed := res.ChangedFields
	if changed == nil {
		changed = []string{}
	}
	audit.SetMetadata(r.Context(), map[string]any{"action": "workbook.rows.bulk_update", "workbook_id": r.PathValue("workbook_id"),
		"updated_rows": res.Updated, "field_names": changed, "reactive_columns": res.Reactive})
	httpx.WriteJSON(w, http.StatusOK, bulkPatchResponse{Status: "updated", UpdatedRows: res.Updated,
		ReactiveColumns: res.Reactive, Recompute: recomputeOut})
}

type leadUpdateResponse struct {
	Status string   `json:"status"`
	LeadID int64    `json:"lead_id"`
	Fields []string `json:"fields"`
}

func (rr rowRoutes) updateLead(w http.ResponseWriter, r *http.Request) {
	var errs []httpx.FieldError
	leadID, fe := wbcrud.PathInt(r.PathValue("lead_id"), "lead_id")
	if fe != nil {
		errs = append(errs, *fe)
	}
	doc, benv, ok := readRowsDoc(w, r)
	if !ok {
		return
	}
	var body *wbcrud.Obj
	if benv != nil {
		errs = append(errs, benv...)
	} else {
		var be []httpx.FieldError
		if body, be = wbcrud.ParseDictBody(doc); be != nil {
			errs = append(errs, be...)
		}
	}
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	var keys []string
	if rr.run(w, r, false, func(ctx context.Context, tx pgx.Tx, ws string, _ wbcrud.Workbook) (*wbcrud.GuardError, error) {
		var (
			g   *wbcrud.GuardError
			err error
		)
		keys, g, err = rr.svc.UpdateLead(ctx, tx, ws, leadID, body)
		return g, err
	}) {
		httpx.WriteJSON(w, http.StatusOK, leadUpdateResponse{Status: "updated", LeadID: leadID, Fields: keys})
	}
}

// trimSpace is Python's str.strip() for the search_applied audit flag.
func trimSpace(s string) string { return wbcrud.TrimPy(s) }
