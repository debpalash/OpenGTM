package api

import (
	"bytes"
	"log/slog"
	"math/big"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// leadStatuses and leadTiers are the constant parts of GET /api/filters.
var (
	leadStatuses = []string{"new", "contacted", "qualified", "negotiating", "converted", "dead"}
	leadTiers    = []string{"hot", "warm", "cold", "unqualified"}
)

// maxListLimit caps GET /api/leads?limit= (decision D2; Python forwards any value to Postgres).
const maxListLimit = 10000

const leadNotFound = "Lead not found"

type leadReadRoutes struct{ d Deps }

// registerReads registers the five lead read routes (routers/leads.py); all sit
// behind current_workspace only, so viewers may read.
func registerReads(r *httpx.Router, d Deps) {
	if d.Pool == nil {
		return
	}
	h := leadReadRoutes{d: d}
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	r.Handle("GET /api/leads", http.HandlerFunc(h.list), authn, workspace)
	r.Handle("GET /api/stats", http.HandlerFunc(h.stats), authn, workspace)
	r.Handle("GET /api/filters", http.HandlerFunc(h.filters), authn, workspace)
	r.Handle("GET /api/lead/{lead_id}", http.HandlerFunc(h.get), authn, workspace)
	r.Handle("GET /api/lead/{lead_id}/similar", http.HandlerFunc(h.similar), authn, workspace)
}

// cleanFilter is leads.py _clean: "" and "__all__" mean unset.
func cleanFilter(s string) string {
	if s == "__all__" {
		return ""
	}
	return s
}

func intParsingError(loc []any, raw string) httpx.FieldError {
	return httpx.FieldError{Type: "int_parsing", Loc: loc, Input: raw,
		Msg: "Input should be a valid integer, unable to parse string as an integer"}
}

// optInt is an `Optional[int]` query parameter. Values outside int32 are clamped: the score columns are int4, so the
// bound keeps its meaning (Python would answer 500 from the database).
func (v *queryValidator) optInt(name string) *int {
	raw, present := v.last(name)
	if !present {
		return nil
	}
	n, ok := wbcrud.ParseIntBig(raw)
	if !ok {
		v.errs = append(v.errs, intParsingError([]any{"query", name}, raw))
		return nil
	}
	lo, hi := big.NewInt(-1<<31), big.NewInt(1<<31-1)
	switch {
	case n.Cmp(lo) < 0:
		n = lo
	case n.Cmp(hi) > 0:
		n = hi
	}
	x := int(n.Int64())
	return &x
}

// optBool is an `Optional[bool]` query parameter.
func (v *queryValidator) optBool(name string) *bool {
	raw, present := v.last(name)
	if !present {
		return nil
	}
	b, ok := parseBoolQuery(raw)
	if !ok {
		v.errs = append(v.errs, httpx.FieldError{Type: "bool_parsing", Loc: []any{"query", name}, Input: raw,
			Msg: "Input should be a valid boolean, unable to interpret input"})
		return nil
	}
	return &b
}

// str is an optional `str` query parameter ("" when absent).
func (v *queryValidator) str(name string) string {
	s, _ := v.last(name)
	return s
}

// nonNegative is an `int` parameter with default def whose negative values answer 422 (D2: Python passes a negative
// LIMIT/OFFSET to Postgres, which fails with a 500). Values above int64 are reported as a bound violation.
func (v *queryValidator) nonNegative(name string, def int64) int64 {
	return v.integer(name, def, i64(0), nil)
}

// listLimit reads `limit` (default 200, >= 0) and caps it at maxListLimit.
func listLimit(v *queryValidator) int {
	n := v.nonNegativeLimit()
	if n > maxListLimit {
		return maxListLimit
	}
	return int(n)
}

func (v *queryValidator) nonNegativeLimit() int64 {
	raw, present := v.last("limit")
	if !present {
		return 200
	}
	// An integer beyond int64 is capped like any other oversized limit.
	if n, ok := wbcrud.ParseIntBig(raw); ok && n.Sign() > 0 && !n.IsInt64() {
		return maxListLimit
	}
	return v.nonNegative("limit", 200)
}

// pathLeadID parses `lead_id: int`. ok=false means a 422 was written. A value outside int64 cannot name a lead.
func pathLeadID(w http.ResponseWriter, r *http.Request, errs []httpx.FieldError) (id int64, none bool, ok bool) {
	raw := r.PathValue("lead_id")
	n, parsed := wbcrud.ParseIntBig(raw)
	if !parsed {
		errs = append([]httpx.FieldError{intParsingError([]any{"path", "lead_id"}, raw)}, errs...)
		httpx.ValidationError(w, errs...)
		return 0, false, false
	}
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return 0, false, false
	}
	if !n.IsInt64() {
		return 0, true, true
	}
	return n.Int64(), false, true
}

func (h leadReadRoutes) log() *slog.Logger {
	if h.d.Log != nil {
		return h.d.Log
	}
	return slog.Default()
}

func (h leadReadRoutes) fail(w http.ResponseWriter, what string, err error) {
	h.log().Error("leads: "+what+" failed", "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

func (h leadReadRoutes) inTx(w http.ResponseWriter, r *http.Request, what string, fn func(pgx.Tx) error) bool {
	wc, _ := identity.WorkspaceFrom(r.Context())
	if err := tenancy.InWorkspace(r.Context(), h.d.Pool, wc.WorkspaceID, fn); err != nil {
		h.fail(w, what, err)
		return false
	}
	return true
}

// writePy marshals v like FastAPI's JSONResponse: compact, UTF-8 as is, including U+2028 and U+2029.
func writePy(w http.ResponseWriter, v any) {
	var buf bytes.Buffer
	rec := &bufResponse{buf: &buf, h: http.Header{}}
	httpx.WriteJSON(rec, http.StatusOK, v)
	if rec.status != http.StatusOK {
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pyJSON(buf.Bytes()))
}

type bufResponse struct {
	buf    *bytes.Buffer
	h      http.Header
	status int
}

func (b *bufResponse) Header() http.Header         { return b.h }
func (b *bufResponse) Write(p []byte) (int, error) { return b.buf.Write(p) }
func (b *bufResponse) WriteHeader(s int)           { b.status = s }

// pyJSON rewrites encoding/json's   and   escapes to the raw characters Python's
// json.dumps(ensure_ascii=False) emits. An escaped backslash followed by "u2028" is left alone.
func pyJSON(b []byte) []byte {
	if !bytes.Contains(b, []byte(`\u202`)) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' || i+1 >= len(b) {
			out = append(out, b[i])
			continue
		}
		if b[i+1] == 'u' && i+5 < len(b) && b[i+2] == '2' && b[i+3] == '0' && b[i+4] == '2' && (b[i+5] == '8' || b[i+5] == '9') {
			if b[i+5] == '8' {
				out = append(out, " "...)
			} else {
				out = append(out, " "...)
			}
			i += 5
			continue
		}
		out = append(out, b[i], b[i+1]) // any other escape, including \\
		i++
	}
	return out
}

func (h leadReadRoutes) list(w http.ResponseWriter, r *http.Request) {
	v := &queryValidator{q: r.URL.Query()}
	p := leadsread.ListParams{}
	status := cleanFilter(v.str("status"))
	p.Status = status
	p.City = cleanFilter(v.str("city"))
	p.Source = cleanFilter(v.str("source"))
	p.ScoreMin = v.optInt("score_min")
	p.ScoreMax = v.optInt("score_max")
	p.ScoreTier = cleanFilter(v.str("tier"))
	p.Search = cleanFilter(v.str("search"))
	// workspace_id is accepted and ignored: the caller's workspace always applies.
	p.HasEmail = v.optBool("has_email")
	p.HasPhone = v.optBool("has_phone")
	p.CompanySize = cleanFilter(v.str("company_size"))
	p.Limit = listLimit(v)
	p.Offset = int(v.nonNegativeOffset())
	p.OrderBy = "score DESC"
	if o, ok := v.last("order_by"); ok {
		p.OrderBy = o
	}
	p.ExcludeDead = status != "dead"
	if v.fail(w) {
		return
	}
	var leads []leadsread.Lead
	if !h.inTx(w, r, "list", func(tx pgx.Tx) (err error) {
		leads, err = leadsread.ListLeads(r.Context(), tx, p)
		return err
	}) {
		return
	}
	if leads == nil {
		leads = []leadsread.Lead{}
	}
	writePy(w, leads)
}

// nonNegativeOffset reads `offset` (default 0, >= 0), capped to int32 range of rows that can exist.
func (v *queryValidator) nonNegativeOffset() int64 {
	raw, present := v.last("offset")
	if !present {
		return 0
	}
	if n, ok := wbcrud.ParseIntBig(raw); ok && n.Sign() > 0 && !n.IsInt64() {
		return 1<<62 - 1 // no row sits past int64; an empty page like Python's
	}
	return v.nonNegative("offset", 0)
}

func (h leadReadRoutes) stats(w http.ResponseWriter, r *http.Request) {
	var res leadsread.StatsResult
	if !h.inTx(w, r, "stats", func(tx pgx.Tx) (err error) {
		res, err = leadsread.Stats(r.Context(), tx)
		return err
	}) {
		return
	}
	writePy(w, res)
}

func (h leadReadRoutes) filters(w http.ResponseWriter, r *http.Request) {
	var opts map[string]any
	if !h.inTx(w, r, "filters", func(tx pgx.Tx) (err error) {
		opts, err = leadsread.FilterOptions(r.Context(), tx)
		return err
	}) {
		return
	}
	writePy(w, struct {
		Cities   any      `json:"cities"`
		Sources  any      `json:"sources"`
		Statuses []string `json:"statuses"`
		Tiers    []string `json:"tiers"`
	}{opts["cities"], opts["sources"], leadStatuses, leadTiers})
}

// oneLead loads lead id inside tx; nil when absent or in another workspace.
func oneLead(r *http.Request, tx pgx.Tx, id int64) (*leadsread.Lead, error) {
	ls, err := leadsread.LeadsByIDs(r.Context(), tx, []int64{id})
	if err != nil || len(ls) == 0 {
		return nil, err
	}
	return &ls[0], nil
}

func (h leadReadRoutes) get(w http.ResponseWriter, r *http.Request) {
	id, none, ok := pathLeadID(w, r, nil)
	if !ok {
		return
	}
	if none {
		httpx.Detail(w, http.StatusNotFound, leadNotFound)
		return
	}
	var lead *leadsread.Lead
	if !h.inTx(w, r, "get", func(tx pgx.Tx) (err error) {
		lead, err = oneLead(r, tx, id)
		return err
	}) {
		return
	}
	if lead == nil {
		httpx.Detail(w, http.StatusNotFound, leadNotFound)
		return
	}
	writePy(w, lead)
}

func (h leadReadRoutes) similar(w http.ResponseWriter, r *http.Request) {
	v := &queryValidator{q: r.URL.Query()}
	limit := v.integer("limit", 10, nil, nil)
	if limit > maxListLimit { // beyond the lead count either way; keeps limit+1 inside int4 for Postgres
		limit = maxListLimit
	}
	id, none, ok := pathLeadID(w, r, v.errs)
	if !ok {
		return
	}
	if none {
		httpx.Detail(w, http.StatusNotFound, leadNotFound)
		return
	}
	var (
		ref     *leadsread.Lead
		similar []leadsread.Lead
	)
	if !h.inTx(w, r, "similar", func(tx pgx.Tx) error {
		var err error
		if ref, err = oneLead(r, tx, id); err != nil || ref == nil {
			return err
		}
		similar, err = leadsread.SimilarLeads(r.Context(), tx, *ref, int(limit))
		return err
	}) {
		return
	}
	if ref == nil {
		httpx.Detail(w, http.StatusNotFound, leadNotFound)
		return
	}
	if similar == nil {
		similar = []leadsread.Lead{}
	}
	writePy(w, struct {
		Reference    string           `json:"reference"`
		Count        int              `json:"count"`
		SimilarLeads []leadsread.Lead `json:"similar_leads"`
	}{ref.Company, len(similar), similar})
}

