package api

import (
	"io"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// RegisterWorkbookDetail registers the read routes of one workbook
// (routers/workbooks.py): GET /api/workbooks/{id} (paged rows),
// GET /api/workbooks/{id}/export.csv and GET /api/workbooks/{id}/connector-runs.
// All are behind current_workspace only (viewers may read).
func RegisterWorkbookDetail(r *httpx.Router, d Deps) {
	if d.Pool == nil || !d.workbooksEnabled("workbook detail") {
		return
	}
	h := wbCRUDRoutes{d: d, svc: &wbcrud.Service{Pool: d.Pool, Leads: wbcrud.PGLeads{}},
		exports: newExportLimiter(d.Flags.ExportMaxConcurrent, d.Flags.ExportMaxPerWorkspace)}
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	r.Handle("GET /api/workbooks/{workbook_id}", http.HandlerFunc(h.detail), authn, workspace)
	r.Handle("GET /api/workbooks/{workbook_id}/export.csv", http.HandlerFunc(h.exportCSV), authn, workspace)
	r.Handle("GET /api/workbooks/{workbook_id}/connector-runs", http.HandlerFunc(h.connectorRuns), authn, workspace)
}

// queryValidator collects FastAPI-style query errors in declaration order.
type queryValidator struct {
	q    url.Values
	errs []httpx.FieldError
}

func (v *queryValidator) last(name string) (string, bool) {
	vals, ok := v.q[name]
	if !ok || len(vals) == 0 {
		return "", false
	}
	return vals[len(vals)-1], true
}

// integer is an `int = Query(default, ge=..., le=...)` parameter. Values beyond int64 are reported as a bound
// violation (FastAPI would pass them to the database and answer 500).
func (v *queryValidator) integer(name string, def int64, ge, le *int64) int64 {
	raw, present := v.last(name)
	if !present {
		return def
	}
	loc := []any{"query", name}
	n, ok := wbcrud.ParseIntBig(raw)
	if !ok {
		v.errs = append(v.errs, httpx.FieldError{Type: "int_parsing", Loc: loc, Input: raw,
			Msg: "Input should be a valid integer, unable to parse string as an integer"})
		return def
	}
	if ge != nil && n.Cmp(big.NewInt(*ge)) < 0 {
		v.errs = append(v.errs, httpx.FieldError{Type: "greater_than_equal", Loc: loc, Input: raw,
			Msg: "Input should be greater than or equal to " + strconv.FormatInt(*ge, 10), Ctx: map[string]any{"ge": *ge}})
		return def
	}
	if le == nil {
		max := int64(math.MaxInt64)
		le = &max
	}
	if n.Cmp(big.NewInt(*le)) > 0 {
		v.errs = append(v.errs, httpx.FieldError{Type: "less_than_equal", Loc: loc, Input: raw,
			Msg: "Input should be less than or equal to " + strconv.FormatInt(*le, 10), Ctx: map[string]any{"le": *le}})
		return def
	}
	if !n.IsInt64() { // below int64 with no lower bound
		v.errs = append(v.errs, httpx.FieldError{Type: "int_parsing", Loc: loc, Input: raw,
			Msg: "Input should be a valid integer, unable to parse string as an integer"})
		return def
	}
	return n.Int64()
}

// text is an optional `str = Query(None, max_length=...)` parameter.
func (v *queryValidator) text(name string, maxLen int) (string, bool) {
	raw, present := v.last(name)
	if !present {
		return "", false
	}
	if maxLen > 0 && utf8.RuneCountInString(raw) > maxLen {
		v.errs = append(v.errs, httpx.FieldError{Type: "string_too_long", Loc: []any{"query", name}, Input: raw,
			Msg: "String should have at most " + strconv.Itoa(maxLen) + " characters", Ctx: map[string]any{"max_length": maxLen}})
		return "", true
	}
	return raw, true
}

func (v *queryValidator) boolean(name string) bool {
	raw, present := v.last(name)
	if !present {
		return false
	}
	b, ok := parseBoolQuery(raw)
	if !ok {
		v.errs = append(v.errs, httpx.FieldError{Type: "bool_parsing", Loc: []any{"query", name}, Input: raw,
			Msg: "Input should be a valid boolean, unable to interpret input"})
	}
	return b
}

func (v *queryValidator) fail(w http.ResponseWriter) bool {
	if len(v.errs) == 0 {
		return false
	}
	httpx.ValidationError(w, v.errs...)
	return true
}

// writeOrdered sends an ordered body as is. encoding/json would re-escape U+2028 and U+2029 even with HTML
// escaping off, which Python's json.dumps(ensure_ascii=False) does not.
func writeOrdered(w http.ResponseWriter, o *wbcrud.Obj) {
	b, err := o.MarshalJSON()
	if err != nil {
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func i64(n int64) *int64 { return &n }

func (h wbCRUDRoutes) detail(w http.ResponseWriter, r *http.Request) {
	v := &queryValidator{q: r.URL.Query()}
	p := wbcrud.DetailParams{}
	p.Page = v.integer("page", 1, i64(1), nil)
	p.PageSize = v.integer("page_size", 100, i64(1), i64(5000))
	p.ViewID, _ = v.text("view_id", 0)
	p.Search, _ = v.text("search", 500)
	p.CursorMode = v.boolean("cursor_mode")
	if c, present := v.text("cursor", 512); present {
		p.Cursor = &c
	}
	if v.fail(w) {
		return
	}
	res, err := h.svc.Detail(r.Context(), h.wsID(r), r.PathValue("workbook_id"), p)
	if err != nil {
		h.fail(w, "detail", err)
		return
	}
	writeOrdered(w, res)
}

func (h wbCRUDRoutes) connectorRuns(w http.ResponseWriter, r *http.Request) {
	v := &queryValidator{q: r.URL.Query()}
	limit := v.integer("limit", 50, i64(1), i64(200))
	offset := v.integer("offset", 0, i64(0), i64(100000))
	cursor, _ := v.text("cursor", 1024)
	if v.fail(w) {
		return
	}
	res, err := h.svc.ConnectorRuns(r.Context(), h.wsID(r), r.PathValue("workbook_id"),
		wbcrud.ConnectorRunsParams{Limit: int(limit), Offset: int(offset), Cursor: cursor})
	if err != nil {
		h.fail(w, "connector runs", err)
		return
	}
	writeOrdered(w, res)
}

func (h wbCRUDRoutes) exportCSV(w http.ResponseWriter, r *http.Request) {
	v := &queryValidator{q: r.URL.Query()}
	p := wbcrud.ExportParams{}
	p.ViewID, _ = v.text("view_id", 0)
	p.Search, _ = v.text("search", 500)
	p.ColumnIDs = v.q["column_ids"]
	for i, raw := range v.q["row_ids"] {
		n, ok := wbcrud.ParseIntLax(raw)
		if !ok {
			v.errs = append(v.errs, httpx.FieldError{Type: "int_parsing", Loc: []any{"query", "row_ids", i}, Input: raw,
				Msg: "Input should be a valid integer, unable to parse string as an integer"})
			continue
		}
		p.RowIDs = append(p.RowIDs, n)
	}
	if v.fail(w) {
		return
	}
	release, ok := h.exports.tryAcquire(h.wsID(r))
	if !ok {
		httpx.Detail(w, http.StatusTooManyRequests, "Too many exports in progress", httpx.Header{Key: "Retry-After", Value: "5"})
		return
	}
	defer release()
	timeout := defaultExportWriteTimeout
	if s := h.d.Flags.ExportWriteTimeoutSeconds; s > 0 {
		timeout = time.Duration(s) * time.Second
	}
	opened := false
	err := h.svc.ExportCSV(r.Context(), h.wsID(r), r.PathValue("workbook_id"), p, func(filename string) io.Writer {
		opened = true
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`.csv"`)
		w.WriteHeader(http.StatusOK)
		return newDeadlineWriter(w, timeout)
	})
	if err == nil {
		return
	}
	if !opened {
		h.fail(w, "export", err)
		return
	}
	// The 200 and part of the body are already sent: cut the connection so the client does not mistake a
	// truncated export for a complete one. A vanished client is not an error worth logging.
	if r.Context().Err() == nil {
		l := h.d.Log
		if l == nil {
			l = slog.Default()
		}
		l.Error("workbooks: export aborted", "err_type", errTypeName(err))
	}
	panic(http.ErrAbortHandler)
}
