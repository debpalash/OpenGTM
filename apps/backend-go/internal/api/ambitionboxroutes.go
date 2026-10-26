package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ambitionbox"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ambitionimport"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// newAmbitionBoxClient builds the gateway client the read routes use (tests swap in a fake
// gateway). Production: the SSRF-guarded egress client with Base pinned to the gateway.
var newAmbitionBoxClient = func(d Deps) *ambitionbox.Client {
	return ambitionbox.NewEgress(d.Config.AppEnv, nil)
}

// ambitionBoxDeadline bounds every read route (Python has none beyond the client's 15 s x 3).
const ambitionBoxDeadline = 20 * time.Second

type abRoutes struct {
	d   Deps
	cl  *ambitionbox.Client
	log *slog.Logger
}

// registerAmbitionBox registers routers/ambitionbox.py: the reads need a workspace; starting an
// import needs the owner, admin or editor role (no permission string, as Python).
func registerAmbitionBox(r *httpx.Router, d Deps) {
	if d.Pool == nil {
		return
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	h := abRoutes{d: d, cl: newAmbitionBoxClient(d), log: log}
	authn, workspace := identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity)
	editor := identity.RequireRole("", "owner", "admin", "editor")
	r.Handle("GET /api/ambitionbox/companies", http.HandlerFunc(h.companies), authn, workspace)
	r.Handle("GET /api/ambitionbox/companies/{company_id}/jobs", http.HandlerFunc(h.jobs), authn, workspace)
	r.Handle("GET /api/ambitionbox/companies/{company_id}/detail", http.HandlerFunc(h.detail), authn, workspace)
	r.Handle("GET /api/ambitionbox/companies/{company_id}/similar", http.HandlerFunc(h.similar), authn, workspace)
	r.Handle("GET /api/ambitionbox/collect", http.HandlerFunc(h.collect), authn, workspace)
	r.Handle("POST /api/ambitionbox/imports", http.HandlerFunc(h.startImport), authn, workspace, editor)
}

// splitList is `value.split(",") if value else None`.
func splitList(raw string, present bool) []string {
	if !present || raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

// pathInt validates `company_id: int` (loc path). Values beyond int are reported as int_parsing.
func pathInt(w http.ResponseWriter, raw string) (int, bool) {
	n, ok := wbcrud.ParseIntBig(raw)
	if !ok || !n.IsInt64() {
		httpx.ValidationError(w, httpx.FieldError{Type: "int_parsing", Loc: []any{"path", "company_id"}, Input: raw,
			Msg: "Input should be a valid integer, unable to parse string as an integer"})
		return 0, false
	}
	return int(n.Int64()), true
}

// upstreamFail maps a client error: unsupported filters are 422 and upstream failures 502 with a
// fixed body (plan 07 D4; Python answers 500). The error text never reaches the client.
func (h abRoutes) upstreamFail(w http.ResponseWriter, r *http.Request, err error) {
	var unsupported *ambitionbox.ErrFilterUnsupported
	var upstream *ambitionbox.UpstreamError
	switch {
	case errors.As(err, &unsupported):
		httpx.Detail(w, http.StatusUnprocessableEntity, unsupported.Error())
	case errors.As(err, &upstream), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		h.log.Warn("ambitionbox: upstream call failed", "path", r.URL.Path, "err_type", errTypeName(err))
		httpx.Detail(w, http.StatusBadGateway, "AmbitionBox upstream error")
	default:
		h.log.Error("ambitionbox: call failed", "path", r.URL.Path, "err_type", errTypeName(err))
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
	}
}

// writeOrderedAny writes an ordered upstream document (object, list or scalar) as is.
func writeOrderedAny(w http.ResponseWriter, status int, v any) {
	if o, ok := v.(*wbcrud.Obj); ok {
		b, err := o.MarshalJSON()
		if err != nil {
			httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(b)
		return
	}
	// lists and scalars: wrap in a throwaway object so the ordered encoder handles nested values
	wrap := wbcrud.NewObj()
	wrap.Set("v", v)
	b, err := wrap.MarshalJSON()
	if err != nil {
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b[len(`{"v":`) : len(b)-1])
}

func (h abRoutes) companies(w http.ResponseWriter, r *http.Request) {
	v := &queryValidator{q: r.URL.Query()}
	page := v.integer("page", 1, i64(1), nil)
	limit := v.integer("limit", 20, i64(1), i64(20))
	if v.fail(w) {
		return
	}
	sortBy, ok := v.last("sort_by")
	if !ok {
		sortBy = "popular"
	}
	industry, hasInd := v.last("industry")
	location, hasLoc := v.last("location")
	ctype, hasType := v.last("company_type")
	rating, _ := v.last("rating")
	ctx, cancel := context.WithTimeout(r.Context(), ambitionBoxDeadline)
	defer cancel()
	res, err := h.cl.SearchCompanies(ctx, ambitionbox.SearchParams{Page: int(page), Limit: int(limit), SortBy: sortBy,
		Industry: splitList(industry, hasInd), Location: splitList(location, hasLoc), CompanyType: splitList(ctype, hasType), Rating: rating})
	if err != nil {
		h.upstreamFail(w, r, err)
		return
	}
	writeOrderedAny(w, http.StatusOK, res)
}

func (h abRoutes) jobs(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r.PathValue("company_id"))
	if !ok {
		return
	}
	v := &queryValidator{q: r.URL.Query()}
	page := v.integer("page", 1, nil, nil)
	if v.fail(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ambitionBoxDeadline)
	defer cancel()
	res, err := h.cl.GetCompanyJobs(ctx, id, int(page))
	if err != nil {
		h.upstreamFail(w, r, err)
		return
	}
	writeOrderedAny(w, http.StatusOK, res)
}

func (h abRoutes) detail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r.PathValue("company_id"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ambitionBoxDeadline)
	defer cancel()
	res, err := h.cl.GetCompanyDetail(ctx, id)
	if err != nil {
		h.upstreamFail(w, r, err)
		return
	}
	writeOrderedAny(w, http.StatusOK, res)
}

func (h abRoutes) similar(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r.PathValue("company_id"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ambitionBoxDeadline)
	defer cancel()
	res, err := h.cl.GetSimilarCompanies(ctx, id)
	if err != nil {
		h.upstreamFail(w, r, err)
		return
	}
	writeOrderedAny(w, http.StatusOK, res)
}

func (h abRoutes) collect(w http.ResponseWriter, r *http.Request) {
	v := &queryValidator{q: r.URL.Query()}
	pages := v.integer("pages", 5, i64(1), i64(25))
	limit := v.integer("limit", 100, i64(1), i64(500))
	if v.fail(w) {
		return
	}
	industry, hasInd := v.last("industry")
	location, hasLoc := v.last("location")
	ctx, cancel := context.WithTimeout(r.Context(), ambitionBoxDeadline)
	defer cancel()
	col, err := h.cl.CollectCompanies(ctx, int(limit), int(pages), splitList(industry, hasInd), splitList(location, hasLoc))
	if err != nil {
		h.upstreamFail(w, r, err)
		return
	}
	out := col.Summary(false)
	companies := make([]any, len(col.Records))
	for i, rec := range col.Records {
		companies[i] = rec.Data
	}
	out.Set("companies", companies)
	out.Set("total", len(col.Records))
	writeOrderedAny(w, http.StatusOK, out)
}

// importRequest is AmbitionBoxImportRequest after validation.
type importRequest struct {
	name                         string
	industry, rating, workbookID *string
	sortBy                       string
	limit                        int
	replace                      bool
}

// optString validates `Optional[str]`: absent or null is nil.
func optString(obj map[string]any, key string, errs *[]httpx.FieldError) *string {
	v, present := obj[key]
	if !present || v == nil {
		return nil
	}
	s, ok := v.(string)
	if !ok {
		*errs = append(*errs, httpx.FieldError{Type: "string_type", Loc: []any{"body", key}, Msg: "Input should be a valid string", Input: v})
		return nil
	}
	return &s
}

// abPyBool mirrors pydantic v2 lax bool validation of a JSON value.
func abPyBool(v any, loc []any) (bool, *httpx.FieldError) {
	bad := func() (bool, *httpx.FieldError) {
		return false, &httpx.FieldError{Type: "bool_parsing", Loc: loc, Input: v, Msg: "Input should be a valid boolean, unable to interpret input"}
	}
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		if b, ok := parseBoolQuery(x); ok {
			return b, nil
		}
		return bad()
	case interface{ String() string }: // json.Number
		f, err := strconv.ParseFloat(x.String(), 64)
		if err == nil && f == 0 {
			return false, nil
		}
		if err == nil && f == 1 {
			return true, nil
		}
		return bad()
	}
	return false, &httpx.FieldError{Type: "bool_type", Loc: loc, Input: v, Msg: "Input should be a valid boolean"}
}

// parseImportRequest validates the body in field declaration order, as pydantic does.
func parseImportRequest(obj map[string]any) (importRequest, []httpx.FieldError) {
	req := importRequest{name: "AmbitionBox Companies", sortBy: "popular", limit: 100}
	var errs []httpx.FieldError
	req.name = fieldString(obj, "name", false, req.name, &errs)
	req.industry = optString(obj, "industry", &errs)
	req.rating = optString(obj, "rating", &errs)
	req.sortBy = fieldString(obj, "sort_by", false, req.sortBy, &errs)
	if _, present := obj["sort_by"]; present {
		if s, isStr := obj["sort_by"].(string); isStr && s != "popular" && s != "rating" && s != "reviews" {
			errs = append(errs, httpx.FieldError{Type: "string_pattern_mismatch", Loc: []any{"body", "sort_by"}, Input: s,
				Msg: "String should match pattern '^(popular|rating|reviews)$'", Ctx: map[string]any{"pattern": "^(popular|rating|reviews)$"}})
		}
	}
	if v, present := obj["limit"]; present {
		loc := []any{"body", "limit"}
		if b, isBool := v.(bool); isBool {
			errs = append(errs, httpx.FieldError{Type: "int_type", Loc: loc, Input: b, Msg: "Input should be a valid integer"})
		} else if n, fe := pyInt(v, loc); fe != nil {
			errs = append(errs, httpx.FieldError{Type: fe.Type, Loc: fe.Loc, Msg: fe.Msg, Input: fe.Input, Ctx: fe.Ctx})
		} else if n.huge || n.v > 500 {
			errs = append(errs, httpx.FieldError{Type: "less_than_equal", Loc: loc, Input: v, Msg: "Input should be less than or equal to 500", Ctx: map[string]any{"le": 500}})
		} else if n.v < 1 {
			errs = append(errs, httpx.FieldError{Type: "greater_than_equal", Loc: loc, Input: v, Msg: "Input should be greater than or equal to 1", Ctx: map[string]any{"ge": 1}})
		} else {
			req.limit = int(n.v)
		}
	}
	req.workbookID = optString(obj, "workbook_id", &errs)
	if v, present := obj["replace_existing"]; present {
		b, fe := abPyBool(v, []any{"body", "replace_existing"})
		if fe != nil {
			errs = append(errs, *fe)
		}
		req.replace = b
	}
	return req, errs
}

func (h abRoutes) startImport(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	obj, ok := readJSONObject(w, r)
	if !ok {
		return
	}
	req, errs := parseImportRequest(obj)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	res, err := ambitionimport.Start(r.Context(), ambitionimport.Deps{Pool: h.d.Pool, Log: h.log,
		Executor: ambitionimport.ExecutorFor(h.d.getenv())}, ambitionimport.StartParams{
		WorkspaceID: wc.WorkspaceID, Name: req.name, Industry: req.industry, Rating: req.rating, SortBy: req.sortBy,
		Limit: req.limit, WorkbookID: req.workbookID, ReplaceExisting: req.replace})
	var running *ambitionimport.AlreadyRunningError
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusAccepted, res)
	case errors.As(err, &running):
		httpx.WriteJSON(w, http.StatusConflict, map[string]any{"detail": orderedPairs{
			{"code", "connector_run_active"}, {"message", running.Error()}, {"run_id", running.RunID}}})
	case errors.Is(err, ambitionimport.ErrWorkbookNotFound):
		httpx.Detail(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ambitionimport.ErrNeedsReplace):
		httpx.Detail(w, http.StatusConflict, err.Error())
	default:
		h.log.Error("ambitionbox: start import failed", "err_type", errTypeName(err))
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
	}
}
