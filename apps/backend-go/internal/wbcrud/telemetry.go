package wbcrud

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Workbook telemetry (routers/workbooks.py: budget, cost, runs, activity, cell
// trace). Responses are built as ordered Obj values so key order and number
// formatting equal FastAPI's json.dumps output.

// NoTraceDetail is get_cell_trace's 404.
const NoTraceDetail = "No trace for this cell"

// PyFloatRepr formats f like Python's repr(float).
func PyFloatRepr(f float64) string { return pyFloat(f) }

// PyRound is Python's round(x, nd): the correctly rounded decimal of the exact
// binary value (half-even only on exact ties), parsed back to a float.
func PyRound(x float64, nd int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	f, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', nd, 64), 64)
	if err != nil {
		return x
	}
	return f
}

// ---- cell_scope.selected_cell_count -------------------------------------

var (
	posIntRE    = regexp.MustCompile(`^[0-9]+$`)
	canonRowKey = regexp.MustCompile(`^[1-9][0-9]*$`)
)

// telJSONInt reports a JSON integer literal (Python int), as its canonical text.
func telJSONInt(v any) (string, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return "", false
	}
	s := string(n)
	if strings.ContainsAny(s, ".eE") {
		return "", false
	}
	return s, true
}

func positiveIntText(v any) (string, bool) {
	s, ok := telJSONInt(v)
	if !ok || !posIntRE.MatchString(s) || strings.Trim(s, "0") == "" {
		return "", false
	}
	return s, true
}

// SelectedCellCountJSON is cell_scope.selected_cell_count over decoded JSON
// values (rows/columns lists, row_columns object; nil for absent or null). The
// result is nil when the stored scope is unknown or malformed.
func SelectedCellCountJSON(rows, columns, rowColumns any) *int {
	rs, ok := rows.([]any)
	if !ok {
		return nil
	}
	cs, ok := columns.([]any)
	if !ok {
		return nil
	}
	rowSet := map[string]bool{}
	for _, r := range rs {
		s, ok := positiveIntText(r)
		if !ok {
			return nil
		}
		rowSet[s] = true
	}
	colSet := map[string]bool{}
	for _, c := range cs {
		s, ok := c.(string)
		if !ok || s == "" {
			return nil
		}
		colSet[s] = true
	}
	if rowColumns == nil {
		n := len(rowSet) * len(colSet)
		return &n
	}
	rc, ok := rowColumns.(*Obj)
	if !ok {
		return nil
	}
	// restrict_work_items' validation: canonical positive row keys, lists of non-empty strings.
	total := 0
	for _, k := range rc.Keys() {
		if !canonRowKey.MatchString(k) {
			return nil
		}
		v, _ := rc.Get(k)
		ids, ok := v.([]any)
		if !ok {
			return nil
		}
		uniq := map[string]bool{}
		for _, id := range ids {
			s, ok := id.(string)
			if !ok || s == "" {
				return nil
			}
			uniq[s] = true
		}
		total += len(uniq)
	}
	if rc.Len() != len(rowSet) {
		return nil
	}
	for _, k := range rc.Keys() {
		if !rowSet[k] {
			return nil
		}
		v, _ := rc.Get(k)
		for _, id := range v.([]any) {
			if !colSet[id.(string)] {
				return nil
			}
		}
	}
	return &total
}

// SelectedCellCount is the typed form: rowColumns nil means "no per-row scope".
func SelectedCellCount(rows, columns any, rowColumns map[string][]string) *int {
	var rc any
	if rowColumns != nil {
		o := NewObj()
		for k, ids := range rowColumns {
			l := make([]any, len(ids))
			for i, id := range ids {
				l[i] = id
			}
			o.Set(k, l)
		}
		rc = o
	}
	return SelectedCellCountJSON(rows, columns, rc)
}

// ---- output_attempts.summarize_output_attempts --------------------------

// OutputAttempts is the public count summary (key order as Python's dict).
type OutputAttempts struct {
	Total           int `json:"total"`
	AwaitingReceipt int `json:"awaiting_receipt"`
	Succeeded       int `json:"succeeded"`
	Failed          int `json:"failed"`
	Unknown         int `json:"unknown"`
}

// SummarizeOutputAttempts counts attempt states; nil (no journal) when v is not an object.
func SummarizeOutputAttempts(v any) *OutputAttempts {
	o, ok := v.(*Obj)
	if !ok {
		return nil
	}
	out := &OutputAttempts{Total: o.Len()}
	for _, k := range o.Keys() {
		a, _ := o.Get(k)
		att, ok := a.(*Obj)
		if !ok {
			out.Unknown++
			continue
		}
		state, _ := att.Get("state")
		res, _ := att.Get("result")
		switch {
		case state == "dispatch_claimed":
			out.AwaitingReceipt++
		case state == "recorded" && isObj(res):
			success, _ := res.(*Obj).Get("success")
			switch success {
			case true:
				out.Succeeded++
			case false:
				out.Failed++
			default:
				out.Unknown++
			}
		default:
			out.Unknown++
		}
	}
	return out
}

func isObj(v any) bool { _, ok := v.(*Obj); return ok }

// ---- Python truthiness / equality on decoded JSON -----------------------

func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case json.Number:
		f, err := strconv.ParseFloat(string(t), 64)
		return err != nil || f != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case *Obj:
		return t.Len() > 0
	}
	return true
}

// pyEqInt is `v == n` for a decoded JSON value and an int (True == 1, 1.0 == 1).
func pyEqInt(v any, n int) bool {
	switch t := v.(type) {
	case bool:
		return (t && n == 1) || (!t && n == 0)
	case json.Number:
		s := string(t)
		if !strings.ContainsAny(s, ".eE") {
			b, ok := new(big.Int).SetString(s, 10)
			return ok && b.IsInt64() && b.Int64() == int64(n)
		}
		f, err := strconv.ParseFloat(s, 64)
		return err == nil && !math.IsInf(f, 0) && f == float64(n)
	}
	return false
}

// ---- cost ---------------------------------------------------------------

// ExposureJSON renders SUM(reserved_microusd)/1000000 the way FastAPI does: the
// driver returns numeric (Decimal), Decimal / 1000000 stays Decimal and
// jsonable_encoder writes an integral Decimal as an int and the rest as a float.
// nil (no rows in those statuses) is the float 0 / 1000000 = 0.0.
func ExposureJSON(micro *int64) string {
	if micro == nil {
		return "0.0"
	}
	if *micro%1000000 == 0 {
		return strconv.FormatInt(*micro/1000000, 10)
	}
	return pyFloat(exposureFloat(micro))
}

func exposureFloat(micro *int64) float64 {
	if micro == nil {
		return 0
	}
	whole, frac := *micro/1000000, *micro%1000000
	sign := ""
	if *micro < 0 {
		sign, whole, frac = "-", -whole, -frac
	}
	f, _ := strconv.ParseFloat(sign+strconv.FormatInt(whole, 10)+"."+leftPad6(frac), 64)
	return f
}

func leftPad6(n int64) string {
	s := strconv.FormatInt(n, 10)
	return strings.Repeat("0", 6-len(s)) + s
}

func sumPresent(m map[string]int64, keys ...string) *int64 {
	var total int64
	present := false
	for _, k := range keys {
		if v, ok := m[k]; ok {
			total += v
			present = true
		}
	}
	if !present {
		return nil
	}
	return &total
}

// BuildCost is get_cost's body. exposure maps the status ("reserved",
// "dispatched", "uncertain") of the rows that exist to SUM(reserved_microusd).
// FastAPI raises TypeError (float - Decimal, a 500) when the cap is positive and
// any exposure row exists; Go answers the figure (documented deviation).
func BuildCost(maxUSD, spentUSD *float64, exposure map[string]int64) *Obj {
	cap, spent := 0.0, 0.0
	if maxUSD != nil {
		cap = *maxUSD
	}
	if spentUSD != nil {
		spent = *spentUSD
	}
	reserved := sumPresent(exposure, "reserved", "dispatched")
	uncertain := sumPresent(exposure, "uncertain")
	o := NewObj()
	o.Set("budget_max_usd", cap)
	o.Set("budget_spent_usd", PyRound(spent, 4))
	o.Set("reserved_usd", json.Number(ExposureJSON(reserved)))
	o.Set("uncertain_usd", json.Number(ExposureJSON(uncertain)))
	if cap > 0 {
		o.Set("remaining_usd", PyRound(cap-spent-exposureFloat(reserved)-exposureFloat(uncertain), 6))
	} else {
		o.Set("remaining_usd", nil)
	}
	o.Set("accounting_basis", "catalog_estimates_and_recorded_charges")
	o.Set("unlimited", cap <= 0)
	return o
}

// ---- runs ---------------------------------------------------------------

// RunJob is one jobs row of type run_workbook.
type RunJob struct {
	ID                                                          int32
	Payload                                                     []byte
	Status                                                      *string
	CreatedAt, StartedAt, CompletedAt, LastHeartbeat, NextRunAt *time.Time
	Error                                                       *string
	RetryCount                                                  *int
}

// isoUTC is timestamp(): naive values are UTC, rendered with isoformat() and a
// +00:00 offset (microseconds only when non-zero).
func isoUTC(t *time.Time) any {
	if t == nil {
		return nil
	}
	u := t.UTC()
	s := u.Format("2006-01-02T15:04:05")
	if us := u.Nanosecond() / 1000; us != 0 {
		s += u.Format(".000000")
	}
	return s + "+00:00"
}

func telStrOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func listLen(v any) any {
	if l, ok := v.([]any); ok {
		return len(l)
	}
	return nil
}

// BuildRunItem is one element of the runs list. Only known execution_result
// fields are exposed, and a result written by a different attempt (retry_count
// differs) is hidden. A payload that is not an object reads as empty (FastAPI
// would raise on a list).
func BuildRunItem(j RunJob) *Obj {
	payload := NewObj()
	if len(j.Payload) > 0 {
		if doc, err := decodeDocument(j.Payload, true); err == nil {
			if o, ok := doc.(*Obj); ok {
				payload = o
			}
		}
	}
	get := func(k string) any { v, _ := payload.Get(k); return v }
	rows := get("row_ids")
	if rows == nil {
		rows = get("lead_ids")
	}
	columns := get("column_ids")
	retry := 0
	if j.RetryCount != nil {
		retry = *j.RetryCount
	}
	var result any
	if res, ok := get("execution_result").(*Obj); ok {
		if rc, _ := res.Get("retry_count"); pyEqInt(rc, retry) {
			r := NewObj()
			for _, k := range []string{"completed", "errors", "total", "rows", "stopped", "recorded_at"} {
				v, _ := res.Get(k)
				r.Set(k, v)
			}
			result = r
		}
	}
	var attempts any
	if a := SummarizeOutputAttempts(get("output_attempts")); a != nil {
		attempts = a
	}
	var selected any
	if n := SelectedCellCountJSON(rows, columns, get("row_columns")); n != nil {
		selected = *n
	}
	o := NewObj()
	o.Set("job_id", int64(j.ID))
	o.Set("run_id", get("run_id"))
	o.Set("status", telStrOrNil(j.Status))
	o.Set("row_count", listLen(rows))
	o.Set("column_count", listLen(columns))
	o.Set("selected_cell_count", selected)
	o.Set("output_attempts", attempts)
	o.Set("fill_missing", pyTruthy(get("fill_missing")))
	o.Set("force", pyTruthy(get("force")))
	o.Set("retry_count", retry)
	o.Set("error", telStrOrNil(j.Error))
	o.Set("created_at", isoUTC(j.CreatedAt))
	o.Set("started_at", isoUTC(j.StartedAt))
	o.Set("completed_at", isoUTC(j.CompletedAt))
	o.Set("last_heartbeat", isoUTC(j.LastHeartbeat))
	o.Set("next_run_at", isoUTC(j.NextRunAt))
	o.Set("result", result)
	return o
}

// ---- service ------------------------------------------------------------

func (s *Service) workbookExists(ctx context.Context, tx pgx.Tx, ws, id string) (gen.GetBudgetFieldsRow, error) {
	row, err := gen.New(tx).GetBudgetFields(ctx, gen.GetBudgetFieldsParams{WorkbookID: id, WorkspaceID: ws})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, guard(404, NotFoundDetail)
	}
	return row, err
}

func f8(v pgtype.Float8) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}

func budgetBody(row gen.GetBudgetFieldsRow) *Obj {
	o := NewObj()
	capv, spent := 0.0, 0.0
	if row.BudgetMaxUsd.Valid {
		capv = row.BudgetMaxUsd.Float64
	}
	if row.BudgetSpentUsd.Valid {
		spent = row.BudgetSpentUsd.Float64
	}
	o.Set("budget_max_usd", capv)
	o.Set("budget_spent_usd", spent)
	return o
}

// SetBudget is set_budget: assigns the ceiling (the ORM stamps updated_at only
// when the value changes, NULL -> 0.0 included) and answers the stored values.
func (s *Service) SetBudget(ctx context.Context, ws, id string, maxUSD float64) (*Obj, error) {
	var out *Obj
	err := s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		if _, err := s.workbookExists(ctx, tx, ws, id); err != nil {
			return err
		}
		q := gen.New(tx)
		if _, err := q.SetBudgetIfChanged(ctx, gen.SetBudgetIfChangedParams{MaxUsd: maxUSD, WorkbookID: id, WorkspaceID: ws}); err != nil {
			return err
		}
		row, err := q.GetBudgetFields(ctx, gen.GetBudgetFieldsParams{WorkbookID: id, WorkspaceID: ws})
		if err != nil {
			return err
		}
		out = budgetBody(row)
		return nil
	})
	return out, err
}

// Cost is get_cost.
func (s *Service) Cost(ctx context.Context, ws, id string) (*Obj, error) {
	var out *Obj
	err := s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		row, err := s.workbookExists(ctx, tx, ws, id)
		if err != nil {
			return err
		}
		sums, err := gen.New(tx).SumSpendExposure(ctx, gen.SumSpendExposureParams{WorkspaceID: ws, WorkbookID: id})
		if err != nil {
			return err
		}
		exposure := map[string]int64{}
		for _, r := range sums {
			exposure[r.Status] = r.Micro
		}
		out = BuildCost(f8(row.BudgetMaxUsd), f8(row.BudgetSpentUsd), exposure)
		return nil
	})
	return out, err
}

// Runs is workbook_runs: newest first, keyset by job id. before nil = first page.
func (s *Service) Runs(ctx context.Context, ws, id string, before *int32, limit int) (*Obj, error) {
	var out *Obj
	err := s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		if _, err := s.workbookExists(ctx, tx, ws, id); err != nil {
			return err
		}
		p := gen.ListRunWorkbookJobsParams{WorkspaceID: ws, WorkbookID: id, RowLimit: int32(limit + 1)}
		if before != nil {
			p.BeforeID = pgtype.Int4{Int32: *before, Valid: true}
		}
		jobs, err := gen.New(tx).ListRunWorkbookJobs(ctx, p)
		if err != nil {
			return err
		}
		hasMore := len(jobs) > limit
		if hasMore {
			jobs = jobs[:limit]
		}
		runs := make([]any, 0, len(jobs))
		for _, j := range jobs {
			rj := RunJob{ID: j.ID, Payload: j.Payload, CreatedAt: ts(j.CreatedAt), StartedAt: ts(j.StartedAt),
				CompletedAt: ts(j.CompletedAt), LastHeartbeat: ts(j.LastHeartbeat), NextRunAt: ts(j.NextRunAt)}
			if j.Status.Valid {
				rj.Status = &j.Status.String
			}
			if j.Error.Valid {
				rj.Error = &j.Error.String
			}
			if j.RetryCount.Valid {
				n := int(j.RetryCount.Int32)
				rj.RetryCount = &n
			}
			runs = append(runs, BuildRunItem(rj))
		}
		o := NewObj()
		o.Set("runs", runs)
		o.Set("has_more", hasMore)
		if hasMore {
			o.Set("next_before_id", int64(jobs[len(jobs)-1].ID))
		} else {
			o.Set("next_before_id", nil)
		}
		out = o
		return nil
	})
	return out, err
}

// Activity is get_activity: newest first (NULL created_at first, as Postgres DESC).
func (s *Service) Activity(ctx context.Context, ws, id string, limit int) (*Obj, error) {
	var out *Obj
	err := s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		if _, err := s.workbookExists(ctx, tx, ws, id); err != nil {
			return err
		}
		rows, err := gen.New(tx).ListWorkbookActivity(ctx, gen.ListWorkbookActivityParams{WorkbookID: id, WorkspaceID: ws, RowLimit: int32(limit)})
		if err != nil {
			return err
		}
		items := make([]any, 0, len(rows))
		for _, r := range rows {
			it := NewObj()
			it.Set("id", int64(r.ID))
			it.Set("kind", pgText(r.Kind))
			it.Set("message", pgText(r.Message))
			if c := naiveTime(ts(r.CreatedAt)); c != nil {
				it.Set("created_at", *c)
			} else {
				it.Set("created_at", nil)
			}
			items = append(items, it)
		}
		o := NewObj()
		o.Set("activity", items)
		out = o
		return nil
	})
	return out, err
}

func pgText(t pgtype.Text) any {
	if !t.Valid {
		return nil
	}
	return t.String
}

// orDefault is Python's `value or default` over a stored json column.
func orDefault(raw []byte, def any) any {
	if len(raw) == 0 {
		return def
	}
	v, err := decodeDocument(raw, true)
	if err != nil || !pyTruthy(v) {
		return def
	}
	return v
}

// CellTrace is get_cell_trace (CellTrace.to_api).
func (s *Service) CellTrace(ctx context.Context, ws, id string, leadID int32, colID string) (*Obj, error) {
	var out *Obj
	err := s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		if _, err := s.workbookExists(ctx, tx, ws, id); err != nil {
			return err
		}
		t, err := gen.New(tx).GetCellTrace(ctx, gen.GetCellTraceParams{WorkbookID: id, WorkspaceID: ws, LeadID: leadID, ColumnID: colID})
		if errors.Is(err, pgx.ErrNoRows) {
			return guard(404, NoTraceDetail)
		}
		if err != nil {
			return err
		}
		o := NewObj()
		o.Set("workbook_id", t.WorkbookID)
		o.Set("lead_id", int64(t.LeadID))
		o.Set("column_id", t.ColumnID)
		o.Set("goal", pgText(t.Goal))
		o.Set("steps", orDefault(t.Steps, []any{}))
		o.Set("outcome", pgText(t.Outcome))
		o.Set("total_cost_usd", orDefault(t.TotalCostUsd, NewObj()))
		out = o
		return nil
	})
	return out, err
}
