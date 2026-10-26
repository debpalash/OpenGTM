package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emaildata"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

// maxDataBody bounds the mark-used and PATCH bodies (hardening; Python has no cap).
const maxDataBody = 1 << 20

// registerDataCRM mounts the 7 /api/data routes. The global-admin gate is applied to
// every route here, in one place, so a route added later without it fails
// TestEveryDataRouteRequiresGlobalAdmin. The tables are global (no workspace_id).
// /export and /stats/* are registered before /{id}; the mux would pick the literal
// segments anyway.
func registerDataCRM(r *httpx.Router, d Deps) {
	h := dataRoutes{svc: emaildata.New(d.Pool), log: d.Log}
	if h.log == nil {
		h.log = slog.Default()
	}
	gate := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.RequireGlobalAdmin()}
	handle := func(pattern string, f http.HandlerFunc) { r.Handle(pattern, f, gate...) }
	handle("GET /api/data", h.list)
	handle("GET /api/data/export", h.export)
	handle("GET /api/data/stats/timeline", h.timeline)
	handle("GET /api/data/stats/domains", h.domains)
	handle("POST /api/data/mark-used", h.markUsed)
	handle("PATCH /api/data/{id}", h.patch)
	handle("DELETE /api/data/{id}", h.remove)
}

type dataRoutes struct {
	svc *emaildata.Service
	log *slog.Logger
}

func (h dataRoutes) fail(w http.ResponseWriter, what string, err error) {
	h.log.Error("data crm: "+what+" failed", "err", err)
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

// serviceErr maps the service sentinels to FastAPI-shaped answers. where is "query" or "body".
func (h dataRoutes) serviceErr(w http.ResponseWriter, what, where string, err error) {
	switch {
	case errors.Is(err, emaildata.ErrInvalidInput):
		httpx.ValidationError(w, httpx.FieldError{Type: "value_error", Loc: []any{where},
			Msg: "Value error, string contains a NUL byte"})
	default:
		h.fail(w, what, err)
	}
}

// ---- lax pydantic scalar parsing ----

const msgIntParsing = "Input should be a valid integer, unable to parse string as an integer"

// laxInt parses an int the way pydantic's lax mode does for strings: surrounding spaces
// are stripped and "5.0" is accepted. Values beyond int64 clamp to the int64 limits (no
// row can have such an id; Python would hit a driver error, see README). The second
// result is the decimal text of the value (for the 404 detail).
func laxInt(raw string) (int64, string, bool) {
	s := strings.TrimSpace(raw)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, strconv.FormatInt(n, 10), true
	}
	bi, ok := new(big.Int).SetString(s, 10)
	if !ok {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) || f != math.Trunc(f) || strings.ContainsAny(s, "_") {
			return 0, "", false
		}
		bf := new(big.Float).SetFloat64(f)
		bi, _ = bf.Int(nil)
	}
	if bi.IsInt64() {
		return bi.Int64(), bi.String(), true
	}
	if bi.Sign() < 0 {
		return math.MinInt64, bi.String(), true
	}
	return math.MaxInt64, bi.String(), true
}

func laxBool(raw string) (bool, bool) {
	switch strings.ToLower(raw) {
	case "1", "true", "t", "yes", "y", "on":
		return true, true
	case "0", "false", "f", "no", "n", "off":
		return false, true
	}
	return false, false
}

// query reads scalar query parameters; the last occurrence wins as in Starlette.
type dataQuery struct {
	r    *http.Request
	errs []httpx.FieldError
}

func (q *dataQuery) raw(name string) (string, bool) {
	v, ok := q.r.URL.Query()[name]
	if !ok || len(v) == 0 {
		return "", false
	}
	return v[len(v)-1], true
}

func (q *dataQuery) intDef(name string, def int64) int64 {
	s, ok := q.raw(name)
	if !ok {
		return def
	}
	n, _, ok := laxInt(s)
	if !ok {
		q.errs = append(q.errs, httpx.FieldError{Type: "int_parsing", Loc: []any{"query", name}, Msg: msgIntParsing, Input: s})
		return def
	}
	return n
}

func (q *dataQuery) optInt(name string) *int64 {
	s, ok := q.raw(name)
	if !ok {
		return nil
	}
	n, _, ok := laxInt(s)
	if !ok {
		q.errs = append(q.errs, httpx.FieldError{Type: "int_parsing", Loc: []any{"query", name}, Msg: msgIntParsing, Input: s})
		return nil
	}
	return &n
}

func (q *dataQuery) optBool(name string) *bool {
	s, ok := q.raw(name)
	if !ok {
		return nil
	}
	b, ok := laxBool(s)
	if !ok {
		q.errs = append(q.errs, httpx.FieldError{Type: "bool_parsing", Loc: []any{"query", name},
			Msg: "Input should be a valid boolean, unable to interpret input", Input: s})
		return nil
	}
	return &b
}

func (q *dataQuery) optStr(name string) *string {
	s, ok := q.raw(name)
	if !ok {
		return nil
	}
	return &s
}

func (q *dataQuery) strDef(name, def string) string {
	if s, ok := q.raw(name); ok {
		return s
	}
	return def
}

// filter reads the parameters list and export share (declaration order of the Python handlers).
func (q *dataQuery) filter() emaildata.Filter {
	f := emaildata.Filter{}
	f.SourceLinkID = q.optInt("source_link_id")
	f.Q = q.optStr("q")
	f.SortBy = q.strDef("sort_by", "id")
	f.Order = q.strDef("order", "desc")
	f.StartDate = q.optStr("start_date")
	f.EndDate = q.optStr("end_date")
	f.IsUsed = q.optBool("is_used")
	return f
}

// dataPathID validates {id}; ok=false when a 422 entry was produced. text is the decimal id for messages.
func dataPathID(r *http.Request) (id int64, text string, errs []httpx.FieldError) {
	raw := r.PathValue("id")
	n, txt, ok := laxInt(raw)
	if !ok {
		return 0, "", []httpx.FieldError{{Type: "int_parsing", Loc: []any{"path", "id"}, Msg: msgIntParsing, Input: raw}}
	}
	return n, txt, nil
}

// ---- JSON bodies (FastAPI shapes) ----

// readObject decodes a required JSON object body. A false result means errs holds the 422 entries.
func readObject(w http.ResponseWriter, r *http.Request) (map[string]any, []httpx.FieldError) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDataBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, []httpx.FieldError{{Type: "value_error", Loc: []any{"body"}, Msg: "Value error, request body too large"}}
		}
		return nil, []httpx.FieldError{{Type: "json_invalid", Loc: []any{"body", 0}, Msg: "JSON decode error", Input: map[string]any{}}}
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, []httpx.FieldError{{Type: "missing", Loc: []any{"body"}, Msg: "Field required"}}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var body any
	if err := dec.Decode(&body); err != nil || dec.More() {
		return nil, []httpx.FieldError{{Type: "json_invalid", Loc: []any{"body", 0}, Msg: "JSON decode error", Input: map[string]any{}}}
	}
	obj, ok := body.(map[string]any)
	if !ok {
		return nil, []httpx.FieldError{{Type: "model_attributes_type", Loc: []any{"body"},
			Msg: "Input should be a valid dictionary or object to extract fields from", Input: body}}
	}
	return obj, nil
}

type dataFields struct {
	obj  map[string]any
	errs []httpx.FieldError
}

func (f *dataFields) bad(typ, msg, name string, input any, extra ...any) {
	loc := append([]any{"body", name}, extra...)
	f.errs = append(f.errs, httpx.FieldError{Type: typ, Loc: loc, Msg: msg, Input: input})
}

// intVal converts one JSON value like pydantic's lax int.
func intVal(v any) (int64, bool, string, string) {
	switch t := v.(type) {
	case json.Number:
		if n, err := strconv.ParseInt(string(t), 10, 64); err == nil {
			return n, true, "", ""
		}
		fl, err := strconv.ParseFloat(string(t), 64)
		if err != nil || math.IsInf(fl, 0) {
			return 0, false, "int_parsing", msgIntParsing
		}
		if fl != math.Trunc(fl) {
			return 0, false, "int_from_float", "Input should be a valid integer, got a number with a fractional part"
		}
		n, _, _ := laxInt(string(t))
		return n, true, "", ""
	case string:
		if n, _, ok := laxInt(t); ok {
			return n, true, "", ""
		}
		return 0, false, "int_parsing", msgIntParsing
	case bool:
		if t {
			return 1, true, "", ""
		}
		return 0, true, "", ""
	}
	return 0, false, "int_type", "Input should be a valid integer"
}

func (f *dataFields) optInt(name string) *int64 {
	v, present := f.obj[name]
	if !present || v == nil {
		return nil
	}
	n, ok, typ, msg := intVal(v)
	if !ok {
		f.bad(typ, msg, name, v)
		return nil
	}
	return &n
}

func (f *dataFields) boolVal(name string, v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case json.Number:
		switch fl, _ := strconv.ParseFloat(string(t), 64); fl {
		case 0:
			return false, true
		case 1:
			return true, true
		}
	case string:
		if b, ok := laxBool(t); ok {
			return b, true
		}
	default:
		f.bad("bool_type", "Input should be a valid boolean", name, v)
		return false, false
	}
	f.bad("bool_parsing", "Input should be a valid boolean, unable to interpret input", name, v)
	return false, false
}

func (f *dataFields) optBool(name string) emaildata.OptBool {
	v, present := f.obj[name]
	if !present {
		return emaildata.OptBool{}
	}
	if v == nil {
		return emaildata.OptBool{Set: true}
	}
	b, ok := f.boolVal(name, v)
	if !ok {
		return emaildata.OptBool{}
	}
	return emaildata.OptBool{Set: true, Val: &b}
}

func (f *dataFields) optStr(name string) emaildata.OptStr {
	v, present := f.obj[name]
	if !present {
		return emaildata.OptStr{}
	}
	if v == nil {
		return emaildata.OptStr{Set: true}
	}
	s, ok := v.(string)
	if !ok {
		f.bad("string_type", "Input should be a valid string", name, v)
		return emaildata.OptStr{}
	}
	return emaildata.OptStr{Set: true, Val: &s}
}

// ---- handlers ----

func (h dataRoutes) list(w http.ResponseWriter, r *http.Request) {
	q := &dataQuery{r: r}
	limit := q.intDef("limit", 50)
	offset := q.intDef("offset", 0)
	f := q.filter()
	if len(q.errs) > 0 {
		httpx.ValidationError(w, q.errs...)
		return
	}
	// Python answers 500 for negative paging; D4: 422, same shape as a ge=0 constraint.
	if errs := emaildata.PagingErrors(limit, offset); errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	page, err := h.svc.List(r.Context(), f, clampInt(limit), clampInt(offset))
	if err != nil {
		h.serviceErr(w, "list", "query", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

// clampInt keeps a huge limit/offset inside int (64-bit platforms: a no-op).
func clampInt(n int64) int {
	if n > math.MaxInt {
		return math.MaxInt
	}
	return int(n)
}

type markUsedAnswer struct {
	Count   int64 `json:"count"`
	Success bool  `json:"success"`
}

func (h dataRoutes) markUsed(w http.ResponseWriter, r *http.Request) {
	obj, errs := readObject(w, r)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	f := &dataFields{obj: obj}
	req := emaildata.MarkUsedRequest{Used: true}
	if v, present := obj["ids"]; present {
		arr, ok := v.([]any)
		if !ok {
			f.bad("list_type", "Input should be a valid list", "ids", v)
		}
		for i, e := range arr {
			n, ok, typ, msg := intVal(e)
			if !ok {
				f.bad(typ, msg, "ids", e, i)
				continue
			}
			req.IDs = append(req.IDs, n)
		}
	}
	if v, present := obj["used"]; present {
		if v == nil {
			f.bad("bool_type", "Input should be a valid boolean", "used", nil)
		} else if b, ok := f.boolVal("used", v); ok {
			req.Used = b
		}
	}
	if o := f.optStr("q"); o.Set {
		req.Q = o.Val
	}
	if o := f.optStr("start_date"); o.Set {
		req.StartDate = o.Val
	}
	if o := f.optStr("end_date"); o.Set {
		req.EndDate = o.Val
	}
	req.SourceLinkID = f.optInt("source_link_id")
	req.IsUsedFilter = f.optBool("is_used_filter").Val
	if len(f.errs) > 0 {
		httpx.ValidationError(w, f.errs...)
		return
	}
	n, err := h.svc.MarkUsed(r.Context(), req)
	if err != nil {
		h.serviceErr(w, "mark-used", "body", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, markUsedAnswer{Count: n, Success: true})
}

type patchAnswer struct {
	Status string        `json:"status"`
	Data   emaildata.Row `json:"data"`
}

func (h dataRoutes) patch(w http.ResponseWriter, r *http.Request) {
	id, idText, errs := dataPathID(r)
	obj, berrs := readObject(w, r)
	errs = append(errs, berrs...)
	var p emaildata.Patch
	if berrs == nil {
		f := &dataFields{obj: obj}
		p = emaildata.Patch{Name: f.optStr("name"), Email: f.optStr("email"), Tags: f.optStr("tags"),
			Notes: f.optStr("notes"), IsUsed: f.optBool("is_used")}
		errs = append(errs, f.errs...)
	}
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	row, err := h.svc.Update(r.Context(), id, p)
	switch {
	case errors.Is(err, emaildata.ErrNotFound):
		httpx.Detail(w, http.StatusNotFound, "Record with id "+idText+" not found")
	case err != nil:
		h.serviceErr(w, "update", "body", err)
	default:
		httpx.WriteJSON(w, http.StatusOK, patchAnswer{Status: "success", Data: row})
	}
}

func (h dataRoutes) remove(w http.ResponseWriter, r *http.Request) {
	id, _, errs := dataPathID(r)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	found, err := h.svc.Delete(r.Context(), id)
	if err != nil {
		h.fail(w, "delete", err)
		return
	}
	st := "not found"
	if found {
		st = "success"
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": st})
}

// lazyResponse defers the status line and headers until the first body byte, so a
// failure before any output (bad input, first query) can still answer a JSON error.
type lazyResponse struct {
	w       http.ResponseWriter
	started bool
	written int64
}

func (l *lazyResponse) Write(p []byte) (int, error) {
	if !l.started {
		l.started = true
		hd := l.w.Header()
		hd.Set("Content-Type", "text/csv; charset=utf-8")
		hd.Set("Content-Disposition", "attachment; filename="+emaildata.ExportFilename(time.Now()))
		l.w.WriteHeader(http.StatusOK)
	}
	n, err := l.w.Write(p)
	l.written += int64(n)
	return n, err
}

// export streams the CSV in service batches; a client disconnect cancels the request
// context (stopping the next batch query) or fails a write, ending the export.
func (h dataRoutes) export(w http.ResponseWriter, r *http.Request) {
	q := &dataQuery{r: r}
	f := q.filter()
	if len(q.errs) > 0 {
		httpx.ValidationError(w, q.errs...)
		return
	}
	lw := &lazyResponse{w: w}
	if err := h.svc.Export(r.Context(), f, lw); err != nil {
		if !lw.started {
			h.serviceErr(w, "export", "query", err)
			return
		}
		// Headers are out: abort the connection so the client sees a broken transfer instead of
		// a clean chunked EOF on a truncated CSV (net/http swallows ErrAbortHandler quietly).
		h.log.Warn("data crm: export aborted", "err", err, "bytes", lw.written)
		panic(http.ErrAbortHandler)
	}
}

func (h dataRoutes) timeline(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.TimelineStats(r.Context())
	if err != nil {
		h.fail(w, "timeline", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h dataRoutes) domains(w http.ResponseWriter, r *http.Request) {
	q := &dataQuery{r: r}
	limit := q.intDef("limit", 10)
	if len(q.errs) > 0 {
		httpx.ValidationError(w, q.errs...)
		return
	}
	out, err := h.svc.DomainStats(r.Context(), clampInt(limit))
	if err != nil {
		h.fail(w, "domains", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
