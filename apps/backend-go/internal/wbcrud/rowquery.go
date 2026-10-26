package wbcrud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// Row query engine: _workbook_value_expression / _workbook_rows_query and the
// keyset paging of the row listing (workbooks.py:110-170, 590-620). All SQL is
// parameterised: column ids, JSON path keys, needles and cursor values are
// bound arguments, never interpolated.

// QueryWorkbook is what the engine needs of a workbook.
type QueryWorkbook struct {
	ID      string
	Columns []Column
}

// QueryView is a loaded saved view.
type QueryView struct {
	ID     string
	Config ViewConfig
}

// DB is the pgx surface the engine reads through (a tenant-scoped pgx.Tx).
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// RowColumns is the select list of RowQuery.SQL, in this order.
const RowColumns = `id, workbook_id, coalesce("position", 0) AS position_c, "position", data, enrichments, lead_id, canonical_entity_id, corroboration_count, created_at, updated_at, workspace_id, source_provider, source_record_id, source_rank, source_fetched_at`

const (
	positionExpr = `coalesce("position", 0)`
	idExpr       = `id`
)

// RowQuery is a built, parameterised row query. SQL selects RowColumns; the
// Order terms are the keyset terms (sort columns, then position, then id).
type RowQuery struct {
	SQL   string
	Args  []any
	Order []OrderTerm

	where     []string
	viewID    *string
	baseArgs  int
	whereArgs int // args referenced by the where conditions (sort terms add more after them)
	baseWhere int
	limit     string
}

type argBuilder struct{ args []any }

func (a *argBuilder) add(v any) string {
	a.args = append(a.args, v)
	return "$" + strconv.Itoa(len(a.args))
}

// valueExpression is _workbook_value_expression: the text value of a column
// in a row, or ok=false for an unknown column.
func valueExpression(cols []Column, columnID string, a *argBuilder) (string, bool) {
	for _, c := range cols {
		if id, ok := idString(c); !ok || id != columnID {
			continue
		}
		if t, _ := c.Get("type").(string); t == "lead_field" || t == "input" {
			key := columnID
			if lf, ok := c.Get("lead_field").(string); ok && lf != "" {
				key = lf
			}
			return "(data ->> " + a.add(key) + "::text)", true
		}
		return "((enrichments -> " + a.add(columnID) + "::text) ->> 'value')", true
	}
	return "", false
}

// likeEscape is SQLAlchemy's autoescape=True with escape character "/".
func likeEscape(s string) string {
	r := strings.NewReplacer("/", "//", "%", "/%", "_", "/_")
	return r.Replace(s)
}

func containsSQL(expr, needleParam string) string {
	return expr + ` LIKE '%' || ` + needleParam + ` || '%' ESCAPE '/'`
}

// BuildRowQuery builds the tenant-owned row query shared by paging and export.
// The caller loads view with LoadView (404 when it is missing or belongs to
// another workbook).
func BuildRowQuery(wb QueryWorkbook, view *QueryView, search string) (RowQuery, *GuardError) {
	a := &argBuilder{}
	q := RowQuery{where: []string{"workbook_id = " + a.add(wb.ID)}}
	var cfg ViewConfig
	if view != nil {
		cfg = view.Config
		id := view.ID
		q.viewID = &id
	}
	for _, rule := range cfg.Filters {
		expr, ok := valueExpression(wb.Columns, rule.Column, a)
		if !ok {
			return RowQuery{}, &GuardError{Status: 409, Detail: "Saved view references a missing filter column. Repair the view before continuing."}
		}
		norm := "lower(coalesce(" + expr + ", ''))"
		expected := strings.ToLower(filterString(rule.Value))
		switch rule.Op {
		case "equals":
			q.where = append(q.where, norm+" = lower("+a.add(expected)+"::text)")
		case "not_equals":
			q.where = append(q.where, norm+" <> lower("+a.add(expected)+"::text)")
		case "contains":
			q.where = append(q.where, containsSQL(norm, "lower("+a.add(likeEscape(expected))+"::text)"))
		case "not_contains":
			q.where = append(q.where, "NOT ("+containsSQL(norm, "lower("+a.add(likeEscape(expected))+"::text)")+")")
		case "empty":
			q.where = append(q.where, "trim("+norm+") = ''")
		case "not_empty":
			q.where = append(q.where, "trim("+norm+") <> ''")
		}
	}
	if needle := strings.ToLower(strings.TrimSpace(search)); needle != "" {
		p := "lower(" + a.add(likeEscape(needle)) + "::text)"
		q.where = append(q.where, "("+containsSQL("lower(data::text)", p)+" OR "+containsSQL("lower(enrichments::text)", p)+")")
	}
	q.whereArgs = len(a.args)
	for _, rule := range cfg.Sort {
		expr, ok := valueExpression(wb.Columns, rule.Column, a)
		if !ok {
			return RowQuery{}, &GuardError{Status: 409, Detail: "Saved view references a missing sort column. Repair the view before continuing."}
		}
		q.Order = append(q.Order, OrderTerm{Expr: "coalesce(" + expr + ", '')", Desc: rule.Dir == "desc"})
	}
	q.Order = append(q.Order, OrderTerm{Expr: positionExpr}, OrderTerm{Expr: idExpr})
	q.Args = a.args
	q.baseArgs = len(a.args)
	q.baseWhere = len(q.where)
	q.rebuild()
	return q, nil
}

func (q RowQuery) orderBy() string {
	parts := make([]string, len(q.Order))
	for i, t := range q.Order {
		parts[i] = t.Expr
		if t.Desc {
			parts[i] += " DESC"
		} else {
			parts[i] += " ASC"
		}
	}
	return strings.Join(parts, ", ")
}

func (q *RowQuery) rebuild() {
	q.SQL = "SELECT " + RowColumns + " FROM workbook_rows WHERE " + strings.Join(q.where, " AND ") +
		" ORDER BY " + q.orderBy() + q.limit
}

// CountSQL counts the rows the query matches (cursor and limit excluded).
func (q RowQuery) CountSQL() (string, []any) {
	return "SELECT count(id) FROM workbook_rows WHERE " + strings.Join(q.where[:q.whereBase()], " AND "), q.Args[:q.whereArgs]
}

// whereBase is the number of non-cursor conditions.
func (q RowQuery) whereBase() int { return q.baseWhere }

// WithLimit appends LIMIT/OFFSET (bound parameters). It never changes the
// scope identity.
func (q RowQuery) WithLimit(limit, offset int) RowQuery {
	a := &argBuilder{args: append([]any{}, q.Args...)}
	q.limit = " LIMIT " + a.add(int64(limit)) + " OFFSET " + a.add(int64(offset))
	q.Args = a.args
	q.rebuild()
	return q
}

// Scope binds a cursor to the workbook, view, effective filters and ordering
// (_workbook_cursor_scope). It is only used by queries with sort terms.
func (q RowQuery) Scope() string {
	n := q.whereBase()
	return scopeHash(q.viewID, strings.Join(q.where[:n], " AND "), q.Args[:q.baseArgs], q.Order)
}

var cursorBadValue = func() *GuardError { return bad400(msgRowCursor) }

// After keeps only the rows after the cursor tuple (_cursor_after_filter):
// values are one text per sort term, then position (null counts as 0), then
// id. A malformed tuple is the 400 row-cursor error.
func (q RowQuery) After(values []any) (RowQuery, *GuardError) {
	if len(values) != len(q.Order) {
		return RowQuery{}, cursorBadValue()
	}
	a := &argBuilder{args: append([]any{}, q.Args...)}
	holders := make([]string, len(values))
	nSort := len(q.Order) - 2
	for i, v := range values {
		switch {
		case i < nSort:
			s, ok := v.(string)
			if !ok || strings.ContainsRune(s, 0) {
				return RowQuery{}, cursorBadValue()
			}
			holders[i] = a.add(s) + "::text"
		default:
			if v == nil && i == nSort {
				v = int64(0)
			}
			n, ok := cursorInt(v)
			if !ok || n > math.MaxInt32 || n < 0 || (i == nSort+1 && n < 1) {
				return RowQuery{}, cursorBadValue()
			}
			holders[i] = a.add(n) + "::int4"
		}
	}
	branches := make([]string, 0, len(values))
	for i, t := range q.Order {
		parts := make([]string, 0, i+1)
		for j := 0; j < i; j++ {
			parts = append(parts, q.Order[j].Expr+" = "+holders[j])
		}
		op := ">"
		if t.Desc {
			op = "<"
		}
		parts = append(parts, t.Expr+" "+op+" "+holders[i])
		branches = append(branches, "("+strings.Join(parts, " AND ")+")")
	}
	q.where = append(append([]string{}, q.where...), "("+strings.Join(branches, " OR ")+")")
	q.Args = a.args
	q.rebuild()
	return q, nil
}

func cursorInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return int64(x), true
		}
	}
	return 0, false
}

// termSelect reads the ordering tuple of one row, scoped like the query.
func (q RowQuery) termSelect() (string, []any) {
	n := q.whereBase()
	exprs := make([]string, len(q.Order))
	for i, t := range q.Order {
		exprs[i] = t.Expr
	}
	args := append([]any{}, q.Args[:q.baseArgs]...)
	return "SELECT " + strings.Join(exprs, ", ") + " FROM workbook_rows WHERE " +
		strings.Join(q.where[:n], " AND ") + " AND id = $" + strconv.Itoa(len(args)+1), args
}

// ValuesOf is the ordering tuple (sort texts, position, id) of row rowID within
// the query's filters; nil when the row is not part of it.
func (q RowQuery) ValuesOf(ctx context.Context, db DB, rowID int64) ([]any, error) {
	sql, args := q.termSelect()
	rows, err := db.Query(ctx, sql, append(args, rowID)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	vals, err := rows.Values()
	if err != nil {
		return nil, err
	}
	for i, v := range vals {
		if n, ok := v.(int32); ok {
			vals[i] = int64(n)
		}
	}
	return vals, rows.Err()
}

// NextCursor encodes the cursor after the last returned row: a row cursor for
// plain position order, otherwise a query cursor (v2 when the tuple is long).
// position is the row's position (0 when null).
func (q RowQuery) NextCursor(ctx context.Context, db DB, lastID int64, position int) (string, error) {
	if len(q.Order) == 2 {
		return EncodeRowCursor(position, lastID), nil
	}
	vals, err := q.ValuesOf(ctx, db, lastID)
	if err != nil {
		return "", err
	}
	if vals == nil {
		return "", fmt.Errorf("cursor row %d not found", lastID)
	}
	return EncodeQueryCursor(vals, lastID, q.Scope()), nil
}

// ResolveCursor turns a client cursor into the ordering tuple for After. The
// guard is the 400 for malformed, foreign-scope or stale cursors.
func (q RowQuery) ResolveCursor(ctx context.Context, db DB, cursor string) ([]any, *GuardError, error) {
	if len(q.Order) == 2 {
		pos, id, g := DecodeRowCursor(cursor)
		if g != nil {
			return nil, g, nil
		}
		return []any{int64(pos), id}, nil, nil
	}
	qc, g := DecodeQueryCursorRaw(cursor, len(q.Order), q.Scope())
	if g != nil {
		return nil, g, nil
	}
	if !qc.Lookup {
		return qc.Values, nil, nil
	}
	vals, err := q.ValuesOf(ctx, db, qc.RowID)
	if err != nil {
		return nil, nil, err
	}
	if vals == nil || ValuesDigest(vals) != qc.Digest {
		return nil, bad400(msgQueryCursor), nil
	}
	return vals, nil, nil
}

// LoadView reads a saved view of workbookID (404 "Workbook view not found"
// when absent or owned by another workbook; any other failure is returned as
// the error, never reported as a missing view). A stored config that no longer
// parses is reported as the missing-filter-column 409 so it is repaired.
func LoadView(ctx context.Context, db DB, workbookID, viewID string) (*QueryView, *GuardError, error) {
	var raw []byte
	err := db.QueryRow(ctx, `SELECT config FROM workbook_views WHERE id = $1 AND workbook_id = $2`, viewID, workbookID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &GuardError{Status: 404, Detail: "Workbook view not found"}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	cfg := ViewConfig{}
	if len(raw) > 0 && string(raw) != "null" {
		var perr []httpx.FieldError
		cfg, perr = ParseStoredViewConfig(raw)
		if perr != nil {
			return nil, &GuardError{Status: 409, Detail: "Saved view references a missing filter column. Repair the view before continuing."}, nil
		}
	}
	return &QueryView{ID: viewID, Config: cfg}, nil, nil
}

// filterString is str("" if value is None else value) for a JSON value.
func filterString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case json.Number:
		switch n := numberValue(x).(type) {
		case int64:
			return strconv.FormatInt(n, 10)
		case float64:
			if s, err := canonjson.FloatRepr(n); err == nil {
				return s
			}
		}
		return x.String()
	case float64:
		if s, err := canonjson.FloatRepr(x); err == nil {
			return s
		}
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *Obj:
		parts := make([]string, 0, x.Len())
		for _, k := range x.Keys() {
			e, _ := x.Get(k)
			parts = append(parts, pyRepr(k)+": "+pyRepr(e))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// pyRepr is Python's repr() for JSON-shaped values.
func pyRepr(v any) string {
	s, ok := v.(string)
	if !ok {
		if v == nil {
			return "None"
		}
		return filterString(v)
	}
	quote := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = `"`
	}
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	s = r.Replace(s)
	if quote == "'" {
		s = strings.ReplaceAll(s, "'", `\'`)
	}
	return quote + s + quote
}
