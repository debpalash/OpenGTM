package api

import (
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/govadmin"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/govaudit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/retention"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

type retentionRoutes struct{ d Deps }

// retentionConfirmation is the phrase an operator must send to purge expired data.
const retentionConfirmation = "PURGE EXPIRED DATA"

// registerGovernanceRetention registers the retention routes (Task 13). All five need governance.manage
// (admin), as Python's require_admin: retention deletes data.
func registerGovernanceRetention(r *httpx.Router, d Deps) {
	h := retentionRoutes{d: d}
	mw := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity), govadmin.Gate("governance.manage")}
	r.Handle("GET /api/governance/retention", http.HandlerFunc(h.get), mw...)
	r.Handle("PUT /api/governance/retention", http.HandlerFunc(h.put), mw...)
	r.Handle("POST /api/governance/retention/preview", http.HandlerFunc(h.preview), mw...)
	r.Handle("POST /api/governance/retention/enforce", http.HandlerFunc(h.enforce), mw...)
	r.Handle("GET /api/governance/retention/runs", http.HandlerFunc(h.runs), mw...)
}

func (h retentionRoutes) executorFor(ws string) string {
	return retention.ExecutorFor(config.ExecutorCohortFrom(h.d.getenv()), h.d.Flags.Governance, ws)
}

// fail never logs request data; only the error type.
func (h retentionRoutes) fail(w http.ResponseWriter, op string, err error) {
	h.d.logger().Error("governance retention: request failed", "op", op, "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

func (h retentionRoutes) ctx(w http.ResponseWriter, r *http.Request) (identity.WorkspaceCtx, bool) {
	c, ok := identity.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
	}
	return c, ok
}

// retentionDefaultPolicy is the GET answer for a workspace without a policy row: no timestamps.
type retentionDefaultPolicy struct {
	WorkspaceID   string         `json:"workspace_id"`
	Enabled       bool           `json:"enabled"`
	LegalHold     bool           `json:"legal_hold"`
	RetentionDays retention.Days `json:"retention_days"`
	NextRunAt     *string        `json:"next_run_at"`
	UpdatedBy     *int32         `json:"updated_by"`
}

func (h retentionRoutes) get(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	var p *retention.Policy
	if err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) (err error) {
		p, err = retention.GetPolicy(r.Context(), tx, c.WorkspaceID)
		return err
	}); err != nil {
		h.fail(w, "get", err)
		return
	}
	if p == nil {
		httpx.WriteJSON(w, http.StatusOK, retentionDefaultPolicy{WorkspaceID: c.WorkspaceID, RetentionDays: retention.Days(retention.DefaultDays)})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// retentionUpdate is the validated RetentionUpdate body. Days keeps the request's key order so a range
// violation is reported for the first offending key, as Python does.
type retentionUpdate struct {
	Enabled   bool
	LegalHold bool
	Days      map[string]any
	Order     []string
}

func retentionBodyObject(raw []byte) (*wbcrud.Obj, []httpx.FieldError) {
	doc, errs := wbcrud.DecodeBody(raw)
	if errs != nil {
		return nil, errs
	}
	o, ok := doc.(*wbcrud.Obj)
	if !ok {
		return nil, []httpx.FieldError{{Type: "model_attributes_type", Loc: []any{"body"}, Input: doc,
			Msg: "Input should be a valid dictionary or object to extract fields from"}}
	}
	return o, nil
}

func retentionMissing(o *wbcrud.Obj, field string) httpx.FieldError {
	return httpx.FieldError{Type: "missing", Loc: []any{"body", field}, Msg: "Field required", Input: o}
}

// parseRetentionUpdate validates `enabled: bool`, `legal_hold: bool = False`,
// `retention_days: dict[str, int] = {}` in pydantic lax mode, collecting every error in field order.
func parseRetentionUpdate(raw []byte) (retentionUpdate, []httpx.FieldError) {
	var out retentionUpdate
	o, errs := retentionBodyObject(raw)
	if errs != nil {
		return out, errs
	}
	if v, ok := o.Get("enabled"); !ok {
		errs = append(errs, retentionMissing(o, "enabled"))
	} else {
		b, e := lenientBool(v, []any{"body", "enabled"})
		out.Enabled, errs = b, append(errs, e...)
	}
	if v, ok := o.Get("legal_hold"); ok {
		b, e := lenientBool(v, []any{"body", "legal_hold"})
		out.LegalHold, errs = b, append(errs, e...)
	}
	out.Days = map[string]any{}
	if v, ok := o.Get("retention_days"); ok {
		days, isObj := v.(*wbcrud.Obj)
		if !isObj {
			errs = append(errs, httpx.FieldError{Type: "dict_type", Loc: []any{"body", "retention_days"}, Input: v,
				Msg: "Input should be a valid dictionary"})
		} else {
			for _, k := range days.Keys() {
				raw, _ := days.Get(k)
				n, e := retentionInt(raw, []any{"body", "retention_days", k})
				if e != nil {
					errs = append(errs, *e)
					continue
				}
				out.Days[k] = n
				out.Order = append(out.Order, k)
			}
		}
	}
	return out, errs
}

// retentionInt is pydantic's lax int for a JSON value: integers, integral floats, numeric strings and
// bools. A value outside int64 is clamped (the range check then rejects it with Python's message).
func retentionInt(v any, loc []any) (int64, *httpx.FieldError) {
	bad := func(typ, msg string) (int64, *httpx.FieldError) {
		return 0, &httpx.FieldError{Type: typ, Loc: loc, Msg: msg, Input: v}
	}
	const parseMsg = "Input should be a valid integer, unable to parse string as an integer"
	clamp := func(b *big.Int) int64 {
		switch {
		case b.IsInt64():
			return b.Int64()
		case b.Sign() < 0:
			return math.MinInt64
		}
		return math.MaxInt64
	}
	fromFloat := func(s string) (int64, *httpx.FieldError) {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return bad("int_parsing", parseMsg)
		}
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return bad("finite_number", "Input should be a finite number")
		}
		if f != math.Trunc(f) {
			return bad("int_from_float", "Input should be a valid integer, got a number with a fractional part")
		}
		b, _ := new(big.Float).SetFloat64(f).Int(nil)
		return clamp(b), nil
	}
	switch t := v.(type) {
	case json.Number:
		s := string(t)
		if !strings.ContainsAny(s, ".eE") {
			b, ok := new(big.Int).SetString(s, 10)
			if !ok {
				return bad("int_parsing", parseMsg)
			}
			return clamp(b), nil
		}
		return fromFloat(s)
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case string:
		s := strings.TrimSpace(t)
		if b, ok := new(big.Int).SetString(s, 10); ok && s != "" && !strings.ContainsAny(s, "_ ") {
			return clamp(b), nil
		}
		if s != "" && strings.Trim(s, "0123456789+-.eE") == "" {
			if n, e := fromFloat(s); e == nil {
				return n, nil
			}
		}
		return bad("int_parsing", parseMsg)
	}
	return bad("int_type", "Input should be a valid integer")
}

func (h retentionRoutes) body(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, ok := wbcrud.ReadBody(r.Body)
	if !ok {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
	}
	return raw, ok
}

func (h retentionRoutes) put(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	body, errs := parseRetentionUpdate(raw)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	days, err := retention.NormalizeDaysOrdered(body.Days, body.Order)
	if err != nil {
		httpx.Detail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	ints := make(map[string]int, len(days))
	for k, v := range days {
		ints[k] = v
	}
	var out retention.Policy
	err = tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) error {
		p, err := retention.UpsertPolicy(r.Context(), tx, c.WorkspaceID, body.Enabled, body.LegalHold, ints, c.User.ID)
		if err != nil {
			return err
		}
		// Reschedule in the same transaction on every update: a hold set or cleared, or the enabled
		// flag flipped, changes the next run, and an unchanged policy just re-arms the same occurrence.
		if _, err := retention.SchedulePolicy(r.Context(), tx, &p, time.Now(), h.executorFor); err != nil {
			return err
		}
		out = p
		return nil
	})
	if err != nil {
		h.fail(w, "put", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h retentionRoutes) preview(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	var out struct {
		RetentionDays retention.Days   `json:"retention_days"`
		LegalHold     bool             `json:"legal_hold"`
		ExpiredCounts retention.Counts `json:"expired_counts"`
	}
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) error {
		p, err := retention.GetPolicy(r.Context(), tx, c.WorkspaceID)
		if err != nil {
			return err
		}
		if p == nil {
			out.RetentionDays = retention.Days(retention.DefaultDays)
		} else if out.RetentionDays, err = retention.StoredDays(p.RetentionDays); err != nil {
			return err
		}
		out.LegalHold = p != nil && p.LegalHold
		ints := make(map[string]int, len(out.RetentionDays))
		for k, v := range out.RetentionDays {
			ints[k] = v
		}
		out.ExpiredCounts, err = retention.Preview(r.Context(), tx, c.WorkspaceID, ints, time.Now())
		return err
	})
	if err != nil {
		h.fail(w, "preview", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h retentionRoutes) enforce(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	o, errs := retentionBodyObject(raw)
	var confirmation string
	if errs == nil {
		v, present := o.Get("confirmation")
		s, isStr := v.(string)
		switch {
		case !present:
			errs = []httpx.FieldError{retentionMissing(o, "confirmation")}
		case !isStr:
			errs = []httpx.FieldError{{Type: "string_type", Loc: []any{"body", "confirmation"}, Msg: "Input should be a valid string", Input: v}}
		default:
			confirmation = s
		}
	}
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	if confirmation != retentionConfirmation {
		httpx.Detail(w, http.StatusUnprocessableEntity, "confirmation must equal "+retentionConfirmation)
		return
	}
	// The run row and its job commit together (Python commits the run first and can leave it
	// pending forever if the enqueue fails: deviation D5).
	var run retention.Run
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) (err error) {
		run, err = retention.RequestRun(r.Context(), tx, c.WorkspaceID, c.User.ID, h.executorFor(c.WorkspaceID))
		return err
	})
	switch {
	case errors.Is(err, retention.ErrNoPolicy), errors.Is(err, retention.ErrLegalHold):
		httpx.Detail(w, http.StatusConflict, err.Error())
	case err != nil:
		h.fail(w, "enforce", err)
	default:
		httpx.WriteJSON(w, http.StatusAccepted, run)
	}
}

// retentionRunsQuery validates limit (default 20, 1..200), offset (default 0, 0..100000) and cursor
// (max_length 1024), reporting errors in declaration order. Last value wins for a repeated parameter.
func retentionRunsQuery(q url.Values) (limit, offset int, cursor string, errs []httpx.FieldError) {
	last := func(k string) (string, bool) {
		v := q[k]
		if len(v) == 0 {
			return "", false
		}
		return v[len(v)-1], true
	}
	bounded := func(name string, def, lo, hi int) int {
		raw, ok := last(name)
		if !ok {
			return def
		}
		loc := []any{"query", name}
		s := strings.Trim(raw, " \t\n\r\v\f")
		b, okInt := retentionQueryInt(s)
		if !okInt {
			errs = append(errs, httpx.FieldError{Type: "int_parsing", Loc: loc, Input: raw,
				Msg: "Input should be a valid integer, unable to parse string as an integer"})
			return def
		}
		switch {
		case b.Cmp(big.NewInt(int64(lo))) < 0:
			errs = append(errs, httpx.FieldError{Type: "greater_than_equal", Loc: loc, Input: raw,
				Msg: "Input should be greater than or equal to " + strconv.Itoa(lo), Ctx: map[string]any{"ge": lo}})
		case b.Cmp(big.NewInt(int64(hi))) > 0:
			errs = append(errs, httpx.FieldError{Type: "less_than_equal", Loc: loc, Input: raw,
				Msg: "Input should be less than or equal to " + strconv.Itoa(hi), Ctx: map[string]any{"le": hi}})
		default:
			return int(b.Int64())
		}
		return def
	}
	limit = bounded("limit", 20, 1, 200)
	offset = bounded("offset", 0, 0, 100000)
	if c, ok := last("cursor"); ok {
		if len([]rune(c)) > govaudit.MaxCursorLen {
			errs = append(errs, httpx.FieldError{Type: "string_too_long", Loc: []any{"query", "cursor"}, Input: c,
				Msg: "String should have at most 1024 characters", Ctx: map[string]any{"max_length": govaudit.MaxCursorLen}})
		}
		cursor = c
	}
	return limit, offset, cursor, errs
}

// retentionQueryInt accepts what pydantic's lax int takes for a query string: an optional sign, digits with single
// underscores between them, and a whole-number ".0" fraction.
func retentionQueryInt(s string) (*big.Int, bool) {
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg, s = s[0] == '-', s[1:]
	}
	ip, fp, hasFrac := strings.Cut(s, ".")
	if hasFrac && (fp == "" || strings.Trim(fp, "0") != "") {
		return nil, false
	}
	if ip == "" || ip[0] == '_' || ip[len(ip)-1] == '_' || strings.Contains(ip, "__") {
		return nil, false
	}
	ip = strings.ReplaceAll(ip, "_", "")
	if strings.Trim(ip, "0123456789") != "" {
		return nil, false
	}
	b, ok := new(big.Int).SetString(ip, 10)
	if !ok {
		return nil, false
	}
	if neg {
		b.Neg(b)
	}
	return b, true
}

func (h retentionRoutes) runs(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	limit, offset, cursor, errs := retentionRunsQuery(r.URL.Query())
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	var page retention.RunPage
	var cerr error
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) error {
		page, cerr = retention.ListRuns(r.Context(), tx, c.WorkspaceID, limit, offset, cursor)
		return nil
	})
	var ce *govaudit.CursorError
	switch {
	case errors.As(cerr, &ce):
		httpx.Detail(w, ce.Status(), ce.Detail)
	case cerr != nil:
		h.fail(w, "runs", cerr)
	case err != nil:
		h.fail(w, "runs", err)
	default:
		httpx.WriteJSON(w, http.StatusOK, page)
	}
}
