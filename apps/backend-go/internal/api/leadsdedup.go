package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leaddedup"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadswrite"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

const (
	dedupDefaultThreshold = 0.85
	dedupDefaultLimit     = 2000
	dedupMaxLimit         = 5000
	mergeRequired         = "master_id and duplicate_ids required"
)

type leadDedupRoutes struct{ d Deps }

// registerDedup registers POST /api/leads/dedup (current_workspace: viewers may run it) and
// POST /api/leads/dedup/merge (require_editor = RequireRole("tables.write", "editor", "admin")).
func registerDedup(r *httpx.Router, d Deps) {
	if d.Pool == nil {
		return
	}
	h := leadDedupRoutes{d: d}
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	r.Handle("POST /api/leads/dedup", http.HandlerFunc(h.run), authn, workspace)
	r.Handle("POST /api/leads/dedup/merge", http.HandlerFunc(h.merge), authn, workspace,
		identity.RequireRole("tables.write", "editor", "admin"))
}

func (h leadDedupRoutes) fail(w http.ResponseWriter, what string, err error) {
	l := h.d.Log
	if l == nil {
		l = slog.Default()
	}
	l.Error("leads dedup: "+what+" failed", "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

// writeRaw sends pre-rendered Python-compatible JSON bytes as is (encoding/json would re-escape them).
func writeRaw(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

var (
	leadKeysOnce sync.Once
	leadKeys     []string
)

// leadRecord is Lead.to_dict() as an ordered dedup record (JSON tag order of leadsread.Lead).
func leadRecord(l leadsread.Lead) leaddedup.Rec {
	leadKeysOnce.Do(func() {
		t := reflect.TypeOf(leadsread.Lead{})
		for i := 0; i < t.NumField(); i++ {
			leadKeys = append(leadKeys, strings.Split(t.Field(i).Tag.Get("json"), ",")[0])
		}
	})
	vals := l.Values()
	rec := leaddedup.Rec{Keys: append([]string(nil), leadKeys...), Vals: make(map[string]any, len(vals))}
	for i, k := range leadKeys {
		rec.Vals[k] = vals[i]
	}
	return rec
}

// threshold is `float = Query(0.85, ge=0.5, le=1.0)`.
func (v *queryValidator) threshold() float64 {
	f := v.optFloat("threshold")
	if f == nil {
		return dedupDefaultThreshold
	}
	raw, _ := v.last("threshold")
	loc := []any{"query", "threshold"}
	switch {
	case !(*f >= 0.5): // also NaN, like pydantic's ge check
		v.errs = append(v.errs, httpx.FieldError{Type: "greater_than_equal", Loc: loc, Input: raw,
			Msg: "Input should be greater than or equal to 0.5", Ctx: map[string]any{"ge": 0.5}})
	case *f > 1.0:
		v.errs = append(v.errs, httpx.FieldError{Type: "less_than_equal", Loc: loc, Input: raw,
			Msg: "Input should be less than or equal to 1", Ctx: map[string]any{"le": json.Number("1.0")}})
	default:
		return *f
	}
	return dedupDefaultThreshold
}

func (h leadDedupRoutes) run(w http.ResponseWriter, r *http.Request) {
	v := &queryValidator{q: r.URL.Query()}
	threshold := v.threshold()
	limit := v.integer("limit", dedupDefaultLimit, i64(1), i64(dedupMaxLimit))
	p := leadsread.ListParams{
		City:      cleanFilter(v.str("city")),
		Source:    cleanFilter(v.str("source")),
		ScoreTier: cleanFilter(v.str("tier")),
		Search:    cleanFilter(v.str("search")),
		Limit:     int(limit),
		OrderBy:   "score DESC",
	}
	if v.fail(w) {
		return
	}
	wc, _ := identity.WorkspaceFrom(r.Context())
	var leads []leadsread.Lead
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, wc.WorkspaceID, func(tx pgx.Tx) (err error) {
		leads, err = leadsread.ListLeads(r.Context(), tx, p)
		return err
	})
	if err != nil {
		h.fail(w, "scan", err)
		return
	}
	if len(leads) == 0 {
		writeRaw(w, http.StatusOK, []byte(`{"stats":{"total_leads":0,"duplicates_found":0},"pairs":[],"clusters":{},"scanned":0}`))
		return
	}
	recs := make([]leaddedup.Rec, len(leads))
	for i, l := range leads {
		recs[i] = leadRecord(l)
	}
	res := leaddedup.FindDuplicates(recs, threshold)
	n := len(leads)
	res.Scanned = &n
	b, err := res.MarshalJSON()
	if err != nil {
		h.fail(w, "encode", err)
		return
	}
	writeRaw(w, http.StatusOK, b)
}

// mergeID is one JSON integer id from the merge body. text is the decimal form echoed in messages; ok is false
// when the value is not a JSON integer (D2: answered like a missing id, 400). n is MinInt64 when out of range
// (no lead can have it).
type mergeID struct {
	n    int64
	text string
	ok   bool
}

func parseMergeID(v any) mergeID {
	num, isNum := v.(json.Number)
	if !isNum || strings.ContainsAny(num.String(), ".eE") {
		return mergeID{}
	}
	n, err := strconv.ParseInt(num.String(), 10, 64)
	if err != nil {
		n = math.MinInt64
	}
	return mergeID{n: n, text: strings.TrimPrefix(num.String(), "+"), ok: true}
}

func (h leadDedupRoutes) merge(w http.ResponseWriter, r *http.Request) {
	body, errs, ok := readLeadBody(w, r)
	if !ok {
		return
	}
	var obj map[string]any
	if errs == nil {
		if obj, ok = body.(map[string]any); !ok {
			errs = []httpx.FieldError{{Type: "dict_type", Loc: []any{"body"}, Msg: "Input should be a valid dictionary", Input: body}}
		}
	}
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	master := parseMergeID(obj["master_id"])
	rawDups, _ := obj["duplicate_ids"].([]any)
	dupIDs := make([]mergeID, len(rawDups))
	valid := master.ok && master.n != 0 && len(rawDups) > 0
	for i, x := range rawDups {
		if dupIDs[i] = parseMergeID(x); !dupIDs[i].ok {
			valid = false
		}
	}
	if !valid {
		httpx.Detail(w, http.StatusBadRequest, mergeRequired)
		return
	}

	wc, _ := identity.WorkspaceFrom(r.Context())
	var (
		status  int
		detail  string
		verr    *httpx.FieldError
		merged  leaddedup.Rec
		deleted int
	)
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, wc.WorkspaceID, func(tx pgx.Tx) error {
		ids := []int64{master.n}
		for _, d := range dupIDs {
			ids = append(ids, d.n)
		}
		found, err := leadsread.LeadsByIDs(r.Context(), tx, ids)
		if err != nil {
			return err
		}
		byID := make(map[int64]leadsread.Lead, len(found))
		for _, l := range found {
			byID[l.ID] = l
		}
		m, ok := byID[master.n]
		if !ok {
			status, detail = http.StatusNotFound, "Master lead "+master.text+" not found"
			return nil
		}
		seen := map[int64]bool{m.ID: true}
		var dups []leaddedup.Rec
		var dupLeadIDs []int64
		hasNonMaster := false
		for _, d := range dupIDs {
			l, ok := byID[d.n]
			if ok && l.ID == m.ID {
				continue
			}
			hasNonMaster = true
			if ok && !seen[l.ID] {
				seen[l.ID] = true
				dups = append(dups, leadRecord(l))
				dupLeadIDs = append(dupLeadIDs, l.ID)
			}
		}
		if !hasNonMaster {
			status, detail = http.StatusBadRequest, "duplicate_ids must contain a lead other than master_id"
			return nil
		}
		if len(dups) == 0 {
			status, detail = http.StatusNotFound, "No valid duplicate leads found"
			return nil
		}
		// D2: where Python crashes with TypeError (500) answer 422, before any write.
		merged, err = leaddedup.MergeLeadsStrict(leadRecord(m), dups)
		switch {
		case errors.Is(err, leaddedup.ErrNonNumericScore):
			verr = &httpx.FieldError{Type: "value_error", Loc: []any{"body", "duplicate_ids"},
				Msg: "Value error, cannot merge leads with a non-numeric score"}
			return nil
		case errors.Is(err, leaddedup.ErrNonStringSource):
			verr = &httpx.FieldError{Type: "value_error", Loc: []any{"body", "duplicate_ids"},
				Msg: "Value error, cannot merge leads with a non-string source"}
			return nil
		case err != nil:
			return err
		}
		// D4: master update and duplicate deletes share this transaction.
		if _, err := leadswrite.UpdateFields(r.Context(), tx, m.ID, merged.Vals); err != nil {
			return err
		}
		for _, id := range dupLeadIDs {
			if _, err := leadswrite.Delete(r.Context(), tx, id); err != nil {
				return err
			}
			deleted++
		}
		status = http.StatusOK
		return nil
	})
	var fe *leadswrite.FieldError
	switch {
	case errors.As(err, &fe):
		httpx.ValidationError(w, httpx.FieldError{Type: "value_error", Loc: []any{"body", fe.Field}, Msg: "Value error, " + fe.Reason})
	case err != nil:
		h.fail(w, "merge", err)
	case verr != nil:
		httpx.ValidationError(w, *verr)
	case status != http.StatusOK:
		httpx.Detail(w, status, detail)
	default:
		mb, err := merged.MarshalJSON()
		if err != nil {
			h.fail(w, "encode", err)
			return
		}
		out := append([]byte(`{"status":"merged","master_id":`+master.text+`,"duplicates_deleted":`+strconv.Itoa(deleted)+`,"merged_lead":`), mb...)
		writeRaw(w, http.StatusOK, append(out, '}'))
	}
}
