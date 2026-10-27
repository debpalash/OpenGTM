package wbcrud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
)

// ---------------------------------------------------------------------------
// Request bodies (schemas.WorkbookCreate / WorkbookUpdate / CreateFromJobs)
// ---------------------------------------------------------------------------

// MaxBodyBytes bounds a workbook JSON request body (a CSV-sourced create can
// carry 50,000 rows).
const MaxBodyBytes = 64 << 20

// CreateBody is a validated WorkbookCreate. Task 22 (templates) builds one
// directly and calls Service.Create.
type CreateBody struct {
	Name        string
	Description string
	// Source is the raw `source` value; Create applies `source or "empty"`.
	Source string
	// SourceConfig is nil when absent or null (`body.source_config or {}`).
	SourceConfig *Obj
	// Columns are already normalised like ColumnConfig.model_dump(exclude_none=True).
	Columns []Column
	// FilterCriteria is the FilterCriteria.model_dump(exclude_none=True) of the
	// request, nil when the request had none.
	FilterCriteria *Obj
	// MaxRows is 1..50000 (default 1000).
	MaxRows int
}

// UpdateBody is a validated WorkbookUpdate. sync_to_leads is validated but,
// like the Python route, never applied.
type UpdateBody struct {
	Name, Description, Status *string
	FilterCriteria            *Obj
	Columns                   *[]Column
}

// FromJobsBody is a validated CreateFromJobsRequest.
type FromJobsBody struct {
	JobIDs     []string
	WorkbookID *string
	Name       *string
}

// DecodeBodyObject turns a raw request body into an ordered object, with the
// pydantic/FastAPI envelope errors (missing body, invalid JSON, not an object).
func DecodeBodyObject(raw []byte) (*Obj, []httpx.FieldError) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, []httpx.FieldError{{Type: "missing", Loc: []any{"body"}, Msg: "Field required", Input: nil}}
	}
	doc, err := decodeDocument(raw, true)
	if err != nil {
		return nil, []httpx.FieldError{{Type: "json_invalid", Loc: []any{"body", 0}, Msg: "JSON decode error", Input: map[string]any{}}}
	}
	obj, ok := doc.(*Obj)
	if !ok {
		return nil, []httpx.FieldError{{Type: "model_attributes_type", Loc: []any{"body"},
			Msg: "Input should be a valid dictionary or object to extract fields from", Input: doc}}
	}
	return obj, nil
}

// ReadBody reads at most MaxBodyBytes; ok is false when the body is larger.
func ReadBody(r io.Reader) ([]byte, bool) {
	b, err := io.ReadAll(io.LimitReader(r, MaxBodyBytes+1))
	if err != nil || len(b) > MaxBodyBytes {
		return nil, false
	}
	return b, true
}

func loc(prefix []any, rest ...any) []any {
	return append(append([]any{}, prefix...), rest...)
}

// strConstrained validates a str field with optional length bounds.
func strConstrained(v any, at []any, minLen, maxLen int) (string, []httpx.FieldError) {
	s, ok := v.(string)
	if !ok {
		return "", fe("string_type", "Input should be a valid string", at, v)
	}
	n := utf8.RuneCountInString(s)
	if minLen > 0 && n < minLen {
		return "", []httpx.FieldError{{Type: "string_too_short", Loc: at, Input: v, Ctx: map[string]any{"min_length": minLen},
			Msg: fmt.Sprintf("String should have at least %d character%s", minLen, plural(minLen))}}
	}
	if maxLen > 0 && n > maxLen {
		return "", []httpx.FieldError{{Type: "string_too_long", Loc: at, Input: v, Ctx: map[string]any{"max_length": maxLen},
			Msg: fmt.Sprintf("String should have at most %d character%s", maxLen, plural(maxLen))}}
	}
	return s, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// filterCriteriaFields is the FilterCriteria model in field order; kind picks the
// pydantic type (all fields are Optional, null means unset).
var filterCriteriaFields = []struct {
	name string
	kind kind
}{
	{"city", kStr}, {"state", kStr}, {"score_tier", kStr}, {"status", kStr}, {"source", kStr}, {"job_ids", kStrList},
	{"specialization", kStr}, {"company_size", kStr}, {"has_email", kBool}, {"has_phone", kBool}, {"has_website", kBool},
	{"min_score", kInt}, {"max_score", kInt}, {"search", kStr},
}

// lenientBool is pydantic's lax bool validation (bool, 0/1, "true"/"no"/...).
func lenientBool(v any, at []any) (bool, []httpx.FieldError) {
	bad := fe("bool_parsing", "Input should be a valid boolean, unable to interpret input", at, v)
	switch t := v.(type) {
	case bool:
		return t, nil
	case json.Number:
		switch string(t) {
		case "0", "0.0":
			return false, nil
		case "1", "1.0":
			return true, nil
		}
		return false, bad
	case string:
		switch strings.ToLower(t) {
		case "0", "off", "f", "false", "n", "no":
			return false, nil
		case "1", "on", "t", "true", "y", "yes":
			return true, nil
		}
		return false, bad
	}
	return false, fe("bool_type", "Input should be a valid boolean", at, v)
}

// parseFilterCriteria validates a FilterCriteria value and returns its
// model_dump(exclude_none=True): known keys only, model field order.
func parseFilterCriteria(v any, at []any) (*Obj, []httpx.FieldError) {
	obj, ok := v.(*Obj)
	if !ok {
		return nil, fe("model_type", "Input should be a valid dictionary or instance of FilterCriteria", at, v)
	}
	out := NewObj()
	var errs []httpx.FieldError
	for _, f := range filterCriteriaFields {
		x, present := obj.Get(f.name)
		if !present || x == nil {
			continue
		}
		fat := loc(at, f.name)
		switch f.kind {
		case kStr:
			if s, ok := x.(string); ok {
				out.Set(f.name, s)
			} else {
				errs = append(errs, fe("string_type", "Input should be a valid string", fat, x)...)
			}
		case kStrList:
			arr, ok := x.([]any)
			if !ok {
				errs = append(errs, fe("list_type", "Input should be a valid list", fat, x)...)
				continue
			}
			bad := false
			for i, e := range arr {
				if _, isStr := e.(string); !isStr {
					errs = append(errs, fe("string_type", "Input should be a valid string", loc(fat, i), e)...)
					bad = true
				}
			}
			if !bad {
				out.Set(f.name, arr)
			}
		case kBool:
			if b, e := lenientBool(x, fat); e != nil {
				errs = append(errs, e...)
			} else {
				out.Set(f.name, b)
			}
		case kInt:
			if n, e := parseInt(x, false, fat); e != nil {
				errs = append(errs, e...)
			} else {
				out.Set(f.name, json.Number(strconv.FormatInt(n, 10)))
			}
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return out, nil
}

// parseColumnList validates a list[ColumnConfig] and normalises every column.
func parseColumnList(v any, at []any) ([]Column, []httpx.FieldError) {
	arr, ok := v.([]any)
	if !ok {
		return nil, fe("list_type", "Input should be a valid list", at, v)
	}
	if max := CurrentLimits().MaxColumns; len(arr) > max {
		return nil, tooLong("List", max, len(arr), at)
	}
	cols := make([]Column, 0, len(arr))
	var errs []httpx.FieldError
	for i, e := range arr {
		raw, err := json.Marshal(e)
		if err != nil {
			return nil, FastAPIModelErrors(fe("model_type", "Input should be a valid dictionary or instance of ColumnConfig", loc(at, i), e))
		}
		c, cerrs := NormalizeRequestColumn(raw, loc(at, i)...)
		if len(cerrs) > 0 {
			errs = append(errs, FastAPIModelErrors(cerrs)...)
			continue
		}
		cols = append(cols, c)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return cols, nil
}

// ParseCreateBody validates a WorkbookCreate request body.
func ParseCreateBody(raw []byte) (CreateBody, []httpx.FieldError) {
	obj, errs := DecodeBodyObject(raw)
	if errs != nil {
		return CreateBody{}, errs
	}
	b := CreateBody{Description: "", Source: "empty", MaxRows: 1000, Columns: []Column{}}
	prefix := []any{"body"}
	add := func(e []httpx.FieldError) { errs = append(errs, e...) }

	if v, ok := obj.Get("name"); !ok {
		add(fe("missing", "Field required", loc(prefix, "name"), obj))
	} else if s, e := strConstrained(v, loc(prefix, "name"), 1, 255); e != nil {
		add(e)
	} else {
		b.Name = s
	}
	if v, ok := obj.Get("description"); ok {
		if s, e := strConstrained(v, loc(prefix, "description"), 0, 2000); e != nil {
			add(e)
		} else {
			b.Description = s
		}
	}
	if v, ok := obj.Get("source"); ok {
		if s, e := strConstrained(v, loc(prefix, "source"), 0, 0); e != nil {
			add(e)
		} else {
			b.Source = s
		}
	}
	if v, ok := obj.Get("source_config"); ok && v != nil {
		if o, isObj := v.(*Obj); isObj {
			b.SourceConfig = o
		} else {
			add(fe("dict_type", "Input should be a valid dictionary", loc(prefix, "source_config"), v))
		}
	}
	if v, ok := obj.Get("columns_config"); ok {
		if cols, e := parseColumnList(v, loc(prefix, "columns_config")); e != nil {
			add(e)
		} else {
			b.Columns = cols
		}
	}
	if v, ok := obj.Get("filter_criteria"); ok && v != nil {
		if fc, e := parseFilterCriteria(v, loc(prefix, "filter_criteria")); e != nil {
			add(e)
		} else {
			b.FilterCriteria = fc
		}
	}
	if v, ok := obj.Get("max_rows"); ok {
		n, e := parseInt(v, false, loc(prefix, "max_rows"))
		switch {
		case len(e) > 0:
			add(e)
		case n < 1:
			add([]httpx.FieldError{{Type: "greater_than_equal", Loc: loc(prefix, "max_rows"), Input: v,
				Msg: "Input should be greater than or equal to 1", Ctx: map[string]any{"ge": 1}}})
		case n > 50000:
			add([]httpx.FieldError{{Type: "less_than_equal", Loc: loc(prefix, "max_rows"), Input: v,
				Msg: "Input should be less than or equal to 50000", Ctx: map[string]any{"le": 50000}}})
		default:
			b.MaxRows = int(n)
		}
	}
	if len(errs) > 0 {
		return CreateBody{}, errs
	}
	return b, nil
}

// ParseUpdateBody validates a WorkbookUpdate request body.
func ParseUpdateBody(raw []byte) (UpdateBody, []httpx.FieldError) {
	obj, errs := DecodeBodyObject(raw)
	if errs != nil {
		return UpdateBody{}, errs
	}
	var b UpdateBody
	prefix := []any{"body"}
	add := func(e []httpx.FieldError) { errs = append(errs, e...) }

	if v, ok := obj.Get("name"); ok && v != nil {
		if s, e := strConstrained(v, loc(prefix, "name"), 1, 255); e != nil {
			add(e)
		} else {
			b.Name = &s
		}
	}
	if v, ok := obj.Get("description"); ok && v != nil {
		if s, e := strConstrained(v, loc(prefix, "description"), 0, 2000); e != nil {
			add(e)
		} else {
			b.Description = &s
		}
	}
	if v, ok := obj.Get("filter_criteria"); ok && v != nil {
		if fc, e := parseFilterCriteria(v, loc(prefix, "filter_criteria")); e != nil {
			add(e)
		} else {
			b.FilterCriteria = fc
		}
	}
	if v, ok := obj.Get("columns_config"); ok && v != nil {
		if cols, e := parseColumnList(v, loc(prefix, "columns_config")); e != nil {
			add(e)
		} else {
			b.Columns = &cols
		}
	}
	if v, ok := obj.Get("status"); ok && v != nil {
		if s, e := strConstrained(v, loc(prefix, "status"), 0, 0); e != nil {
			add(e)
		} else {
			b.Status = &s
		}
	}
	if v, ok := obj.Get("sync_to_leads"); ok && v != nil {
		if _, e := lenientBool(v, loc(prefix, "sync_to_leads")); e != nil {
			add(e)
		}
	}
	if len(errs) > 0 {
		return UpdateBody{}, errs
	}
	return b, nil
}

// ParseFromJobsBody validates a CreateFromJobsRequest body.
func ParseFromJobsBody(raw []byte) (FromJobsBody, []httpx.FieldError) {
	obj, errs := DecodeBodyObject(raw)
	if errs != nil {
		return FromJobsBody{}, errs
	}
	var b FromJobsBody
	prefix := []any{"body"}
	add := func(e []httpx.FieldError) { errs = append(errs, e...) }

	at := loc(prefix, "job_ids")
	if v, ok := obj.Get("job_ids"); !ok {
		add(fe("missing", "Field required", at, obj))
	} else if arr, isArr := v.([]any); !isArr {
		add(fe("list_type", "Input should be a valid list", at, v))
	} else {
		ids := make([]string, 0, len(arr))
		var itemErrs []httpx.FieldError
		for i, e := range arr {
			s, isStr := e.(string)
			if !isStr {
				itemErrs = append(itemErrs, fe("string_type", "Input should be a valid string", loc(at, i), e)...)
				continue
			}
			ids = append(ids, s)
		}
		switch {
		case len(itemErrs) > 0:
			add(itemErrs)
		case len(ids) < 1:
			add([]httpx.FieldError{{Type: "too_short", Loc: at, Input: v,
				Msg: "List should have at least 1 item after validation, not 0",
				Ctx: map[string]any{"field_type": "List", "min_length": 1, "actual_length": 0}}})
		default:
			b.JobIDs = ids
		}
	}
	for _, k := range []string{"workbook_id", "name"} {
		v, ok := obj.Get(k)
		if !ok || v == nil {
			continue
		}
		maxLen := 0
		if k == "name" {
			maxLen = 255 // varchar(255): FastAPI answers 500 on longer names, Go 422 (hardening)
		}
		s, e := strConstrained(v, loc(prefix, k), 0, maxLen)
		if e != nil {
			add(e)
			continue
		}
		if k == "workbook_id" {
			b.WorkbookID = &s
		} else {
			b.Name = &s
		}
	}
	if len(errs) > 0 {
		return FromJobsBody{}, errs
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

// snapshotRow is one workbook_rows insert.
type snapshotRow struct {
	Position int
	LeadID   int64 // 0 = none
	Data     []byte
}

// insertSnapshotRows bulk-inserts rows in order and returns the new row ids.
func insertSnapshotRows(ctx context.Context, tx pgx.Tx, ws, workbookID string, rows []snapshotRow) ([]int64, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	type item struct {
		P int    `json:"p"`
		L int64  `json:"l"`
		D string `json:"d"`
	}
	items := make([]item, len(rows))
	for i, r := range rows {
		items[i] = item{P: r.Position, L: r.LeadID, D: string(r.Data)}
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	ids, err := gen.New(tx).InsertWorkbookRows(ctx, gen.InsertWorkbookRowsParams{WorkbookID: workbookID, WorkspaceID: ws, Items: raw})
	if err != nil {
		return nil, err
	}
	out := make([]int64, len(ids))
	for i, id := range ids {
		out[i] = int64(id)
	}
	return out, nil
}

// leadSnapshotRows turns leads into snapshot rows at positions start+index.
func leadSnapshotRows(leads []leadsread.Lead, start int, skip map[int64]bool) ([]snapshotRow, error) {
	rows := make([]snapshotRow, 0, len(leads))
	for i, l := range leads {
		id := l.ID
		if id != 0 && skip[id] {
			continue
		}
		data, err := json.Marshal(l)
		if err != nil {
			return nil, err
		}
		rows = append(rows, snapshotRow{Position: start + i, LeadID: id, Data: data})
	}
	return rows, nil
}

// filterFromObj decodes a FilterCriteria-shaped object into a lead filter
// (unknown keys ignored).
func filterFromObj(o *Obj) (leadsread.Filter, error) {
	var f leadsread.Filter
	if o == nil {
		return f, nil
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return f, err
	}
	err = json.Unmarshal(raw, &f)
	return f, err
}

// Create is create_workbook: it validates the columns (duplicate ids and new
// dependency cycles answer a *GuardError with status 422), inserts the
// workbook, snapshots rows per source and fires the row-added automation hook
// (failures swallowed), all in one workspace transaction. The returned
// workbook carries the live row count. Task 22 (templates) reuses this.
func (s *Service) Create(ctx context.Context, ws string, body CreateBody) (Workbook, error) {
	seen := map[string]struct{}{}
	for _, c := range body.Columns {
		id := pyStr(c.Get("id"))
		if _, dup := seen[id]; dup {
			return Workbook{}, &GuardError{Status: 422, Detail: "Column IDs must be unique"}
		}
		seen[id] = struct{}{}
	}
	if g := ValidateNewCycles(nil, body.Columns); g != nil {
		return Workbook{}, g
	}

	source := body.Source
	if source == "" {
		source = "empty"
	}
	filter := body.FilterCriteria
	if filter == nil {
		filter = NewObj()
	}
	cfg := body.SourceConfig
	if cfg == nil {
		cfg = NewObj()
	}
	// Legacy compat: a filter without an explicit source means leads_filter.
	if filter.Len() > 0 && source == "empty" {
		source = "leads_filter"
		cfg = filter
	}
	stored := NewObj()
	for _, k := range cfg.Keys() {
		v, _ := cfg.Get(k)
		stored.Set(k, v)
	}
	stored.Set("row_storage_version", json.Number("2"))
	maxRows := body.MaxRows
	if maxRows <= 0 {
		maxRows = 1000
	}

	cols := body.Columns
	if cols == nil {
		cols = []Column{}
	}
	var out Workbook
	err := s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		scJSON, err := json.Marshal(stored)
		if err != nil {
			return err
		}
		fcJSON, err := json.Marshal(filter)
		if err != nil {
			return err
		}
		colJSON, err := json.Marshal(cols)
		if err != nil {
			return err
		}
		row, err := gen.New(tx).InsertWorkbook(ctx, gen.InsertWorkbookParams{
			ID: NewID(), Name: body.Name, Description: body.Description, WorkspaceID: ws, SourceType: source,
			SourceConfig: scJSON, FilterCriteria: fcJSON, ColumnsConfig: colJSON,
		})
		if err != nil {
			return err
		}
		wb, err := FromRow(row)
		if err != nil {
			return err
		}

		rows, err := s.sourceRows(ctx, tx, source, filter, cfg, maxRows)
		if err != nil {
			return err
		}
		ids, err := insertSnapshotRows(ctx, tx, ws, wb.ID, rows)
		if err != nil {
			return err
		}
		if len(ids) > 0 && s.Hook != nil {
			// on_row_added emit failures never fail the create (Python logs a warning).
			_, _ = s.Hook.RowsAdded(ctx, tx, ws, wb.ID, ids)
		}
		s.Hydrate(ctx, tx, ws, &wb, false)
		out = wb
		return nil
	})
	if err != nil {
		return Workbook{}, err
	}
	return out, nil
}

// sourceRows builds the snapshot rows of a new workbook for its source type.
func (s *Service) sourceRows(ctx context.Context, tx pgx.Tx, source string, filter, cfg *Obj, maxRows int) ([]snapshotRow, error) {
	switch source {
	case "leads_filter":
		f, err := filterFromObj(filter)
		if err != nil {
			return nil, err
		}
		return s.leadRows(ctx, tx, f, maxRows)
	case "job_results":
		// Non-list job_ids would make FastAPI's lead query fail (500); Go snapshots nothing.
		v, _ := cfg.Get("job_ids")
		arr, _ := v.([]any)
		if len(arr) == 0 {
			return nil, nil
		}
		ids := make([]string, len(arr))
		for i, e := range arr {
			ids[i] = pyStr(e)
		}
		return s.leadRows(ctx, tx, leadsread.Filter{JobIDs: ids}, maxRows)
	case "csv":
		// A non-list `rows` makes FastAPI slice a non-list (500); Go snapshots nothing.
		v, _ := cfg.Get("rows")
		arr, _ := v.([]any)
		if len(arr) > maxRows {
			arr = arr[:maxRows]
		}
		rows := make([]snapshotRow, 0, len(arr))
		for i, e := range arr {
			data, err := json.Marshal(e)
			if err != nil {
				return nil, err
			}
			rows = append(rows, snapshotRow{Position: i, Data: data})
		}
		return rows, nil
	}
	return nil, nil
}

func (s *Service) leadRows(ctx context.Context, tx pgx.Tx, f leadsread.Filter, limit int) ([]snapshotRow, error) {
	if s.Leads == nil {
		return nil, errors.New("wbcrud: no lead reader configured")
	}
	leads, _, err := s.Leads.QueryLeads(ctx, tx, f, 1, limit)
	if err != nil {
		return nil, err
	}
	return leadSnapshotRows(leads, 0, nil)
}

// ---------------------------------------------------------------------------
// Update, delete, from-jobs
// ---------------------------------------------------------------------------

// viewColumnRefs is the set of column ids a stored view config references
// (filters, sort and hidden_columns); malformed parts are skipped.
func viewColumnRefs(raw []byte) map[string]struct{} {
	refs := map[string]struct{}{}
	doc, err := decodeDocument(raw, false)
	if err != nil {
		return refs
	}
	cfg, ok := doc.(*Obj)
	if !ok {
		return refs
	}
	for _, key := range []string{"filters", "sort"} {
		v, _ := cfg.Get(key)
		arr, _ := v.([]any)
		for _, rule := range arr {
			if o, ok := rule.(*Obj); ok {
				if c, ok := o.Get("column"); ok && c != nil {
					refs[pyStr(c)] = struct{}{}
				}
			}
		}
	}
	v, _ := cfg.Get("hidden_columns")
	arr, _ := v.([]any)
	for _, c := range arr {
		if c != nil {
			refs[pyStr(c)] = struct{}{}
		}
	}
	return refs
}

// checkReplacement is the column-replacement guard of update_workbook: unique
// ids (422), rename and cycle guards, then refusing to drop a column that
// another column or a saved view still references (409).
func (s *Service) checkReplacement(ctx context.Context, tx pgx.Tx, ws string, w Workbook, repl []Column) error {
	ids := map[string]struct{}{}
	for _, c := range repl {
		id := pyStr(c.Get("id"))
		if _, dup := ids[id]; dup {
			return &GuardError{Status: 422, Detail: "Column IDs must be unique"}
		}
		ids[id] = struct{}{}
	}
	if g := ValidateRenames(w.Columns, repl); g != nil {
		return g
	}
	if g := ValidateNewCycles(w.Columns, repl); g != nil {
		return g
	}
	var removed []Column
	removedIDs := map[string]struct{}{}
	for _, c := range w.Columns {
		if _, kept := ids[pyStr(c.Get("id"))]; !kept {
			removed = append(removed, c)
			removedIDs[pyStr(c.Get("id"))] = struct{}{}
		}
	}
	if len(removed) == 0 {
		return nil
	}
	for _, c := range removed {
		if len(ReferencingColumns(repl, c)) > 0 {
			return &GuardError{Status: 409, Detail: "Replacement removes a referenced column. Update dependent columns first."}
		}
	}
	views, err := gen.New(tx).ListWorkbookViewConfigs(ctx, gen.ListWorkbookViewConfigsParams{WorkbookID: w.ID, WorkspaceID: ws})
	if err != nil {
		return err
	}
	for _, v := range views {
		for ref := range viewColumnRefs(v.Config) {
			if _, hit := removedIDs[ref]; hit {
				return &GuardError{Status: 409, Detail: "Replacement removes a column referenced by saved view: " + v.Name + ". Update the view first."}
			}
		}
	}
	return nil
}

// Update is update_workbook: the workbook is locked, the column replacement is
// guarded, and only fields whose value really changed are written (and stamp
// updated_at). Errors: ErrNotFound, *GuardError.
func (s *Service) Update(ctx context.Context, ws, id string, body UpdateBody) (Workbook, error) {
	var out Workbook
	err := s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		w, err := s.Owned(ctx, tx, ws, id, true)
		if err != nil {
			return err
		}
		if body.Columns != nil {
			if err := s.checkReplacement(ctx, tx, ws, w, *body.Columns); err != nil {
				return err
			}
		}
		patch := UpdatePatch{Name: body.Name, Description: body.Description, Status: body.Status, Columns: body.Columns}
		if body.FilterCriteria != nil {
			patch.SetFilterCriteria, patch.FilterCriteria = true, body.FilterCriteria
		}
		w, _, err = s.UpdateIfChanged(ctx, tx, ws, w, patch)
		if err != nil {
			return err
		}
		s.Hydrate(ctx, tx, ws, &w, true)
		out = w
		return nil
	})
	return out, err
}

// Delete is delete_workbook; rows, views, enrichments and the other children
// go with it through their ON DELETE CASCADE foreign keys.
func (s *Service) Delete(ctx context.Context, ws, id string) error {
	return s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		if _, err := s.Owned(ctx, tx, ws, id, false); err != nil {
			return err
		}
		_, err := gen.New(tx).DeleteWorkbook(ctx, gen.DeleteWorkbookParams{WorkbookID: id, WorkspaceID: ws})
		return err
	})
}

// fromJobsColumns are the default columns of a workbook created from jobs.
func fromJobsColumns() []Column {
	spec := []struct {
		id, name string
		width    string
	}{
		{"company", "Company", "220"}, {"specialization", "Specialization", "180"}, {"contact_person", "Contact", "180"},
		{"contact_title", "Title", "150"}, {"email", "Email", "220"}, {"phone", "Phone", "160"},
		{"website", "Website", "180"}, {"linkedin_url", "LinkedIn", "180"}, {"city", "City", "130"},
		{"state", "State", "120"}, {"company_size", "Size", "100"}, {"industry_tags", "Industry", "180"},
		{"description", "Description", "250"}, {"score", "Score", "80"}, {"status", "Status", "100"},
		{"source", "Source", "120"}, {"notes", "Notes", "200"}, {"created_at", "Created", "140"},
	}
	cols := make([]Column, len(spec))
	for i, c := range spec {
		cols[i] = MakeColumn("id", c.id, "name", c.name, "type", "lead_field", "lead_field", c.id, "width", json.Number(c.width))
	}
	return cols
}

// FromJobs is create_workbook_from_jobs: it merges job ids into an existing
// workbook (body.WorkbookID) or creates a job_results workbook, then snapshots
// the matching leads that are not yet rows. Errors: ErrNotFound.
//
// Merging always persists the union of job ids into the stored filter and keeps its other keys
// (Python lost the merge unless the stored filter was empty; F3). Faithful Python quirks, pinned
// by the recorded fixtures:
//   - A new workbook's auto name cannot consult jobs (they lived in the
//     per-workspace SQLite file; Go is Postgres-only), so it is always
//     "Task Workbook (N jobs)".
//   - New rows are positioned at existing_count + index in the lead list even
//     when earlier leads were skipped as duplicates.
//
// The automation row-added hook is deliberately not fired (Python does not).
func (s *Service) FromJobs(ctx context.Context, ws string, body FromJobsBody) (Workbook, error) {
	var out Workbook
	err := s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		var w Workbook
		if body.WorkbookID != nil && *body.WorkbookID != "" {
			cur, err := s.Owned(ctx, tx, ws, *body.WorkbookID, false)
			if err != nil {
				return err
			}
			existing, _ := cur.FilterCriteria.(*Obj)
			if existing == nil {
				existing = NewObj()
			}
			set := map[string]struct{}{}
			if v, ok := existing.Get("job_ids"); ok {
				arr, _ := v.([]any)
				for _, e := range arr {
					set[pyStr(e)] = struct{}{}
				}
			}
			for _, id := range body.JobIDs {
				set[id] = struct{}{}
			}
			merged := make([]string, 0, len(set))
			for id := range set {
				merged = append(merged, id)
			}
			sortStrings(merged)
			existing.Set("job_ids", merged) // other filter keys are kept
			raw, err := json.Marshal(existing)
			if err != nil {
				return err
			}
			if _, err := q.UpdateWorkbookMeta(ctx, gen.UpdateWorkbookMetaParams{
				WorkbookID: cur.ID, WorkspaceID: ws, SetFilterCriteria: true, FilterCriteria: raw,
			}); err != nil {
				return err
			}
			if w, err = s.Owned(ctx, tx, ws, cur.ID, false); err != nil {
				return err
			}
		} else {
			name := ""
			if body.Name != nil {
				name = *body.Name
			}
			if name == "" {
				name = fmt.Sprintf("Task Workbook (%d jobs)", len(body.JobIDs))
			}
			fc := NewObj()
			fc.Set("job_ids", body.JobIDs)
			fcJSON, err := json.Marshal(fc)
			if err != nil {
				return err
			}
			colJSON, err := json.Marshal(fromJobsColumns())
			if err != nil {
				return err
			}
			row, err := q.InsertWorkbook(ctx, gen.InsertWorkbookParams{
				ID: NewID(), Name: name, Description: fmt.Sprintf("Created from %d chat task(s)", len(body.JobIDs)),
				WorkspaceID: ws, SourceType: "job_results", SourceConfig: fcJSON, FilterCriteria: fcJSON, ColumnsConfig: colJSON,
			})
			if err != nil {
				return err
			}
			if w, err = FromRow(row); err != nil {
				return err
			}
		}

		// Snapshot leads that are not rows yet.
		fobj, _ := w.FilterCriteria.(*Obj)
		f, err := filterFromObj(fobj)
		if err != nil {
			return err
		}
		if s.Leads == nil {
			return errors.New("wbcrud: no lead reader configured")
		}
		leads, _, err := s.Leads.QueryLeads(ctx, tx, f, 1, 5000)
		if err != nil {
			return err
		}
		count, err := q.CountWorkbookRows(ctx, gen.CountWorkbookRowsParams{WorkbookID: w.ID, WorkspaceID: ws})
		if err != nil {
			return err
		}
		have, err := q.ListWorkbookRowLeadIDs(ctx, gen.ListWorkbookRowLeadIDsParams{WorkbookID: w.ID, WorkspaceID: ws})
		if err != nil {
			return err
		}
		skip := make(map[int64]bool, len(have))
		for _, id := range have {
			skip[int64(id)] = true
		}
		rows, err := leadSnapshotRows(leads, int(count), skip)
		if err != nil {
			return err
		}
		if _, err := insertSnapshotRows(ctx, tx, ws, w.ID, rows); err != nil {
			return err
		}
		s.Hydrate(ctx, tx, ws, &w, true)
		out = w
		return nil
	})
	return out, err
}

func sortStrings(a []string) { sort.Strings(a) }
